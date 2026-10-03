package strategy

import (
	"fmt"
	"math"
	"sort"
	"time"

	"agentic-trader/internal/store"
)

// MemData serves candles from memory as of a moving "now". Used by backtests
// and by live ticks (loaded from the store).
type MemData struct {
	Now  time.Time
	bars map[string][]store.Candle // "SYM/interval" -> bars by start, ascending
}

// NewMemData indexes candles for lookups.
func NewMemData(candles []store.Candle) *MemData {
	d := &MemData{bars: map[string][]store.Candle{}}
	for _, c := range candles {
		k := c.Symbol + "/" + c.Interval
		d.bars[k] = append(d.bars[k], c)
	}
	for _, bs := range d.bars {
		sort.Slice(bs, func(i, j int) bool { return bs[i].Start.Before(bs[j].Start) })
	}
	return d
}

// Closes implements Data: closes of bars that closed at or before Now.
func (d *MemData) Closes(symbol, interval string, n int) []float64 {
	bs := d.bars[symbol+"/"+interval]
	dur := Intervals[interval]
	// Index of the first bar not yet closed at Now.
	end := sort.Search(len(bs), func(i int) bool { return bs[i].Start.Add(dur).After(d.Now) })
	start := max(end-n, 0)
	out := make([]float64, 0, end-start)
	for _, c := range bs[start:end] {
		out = append(out, c.Close)
	}
	return out
}

// PriceAt is the latest close at or before Now (closed bars only).
func (d *MemData) PriceAt(symbol, interval string) (float64, bool) {
	c := d.Closes(symbol, interval, 1)
	if len(c) == 0 {
		return 0, false
	}
	return c[0], true
}

// BacktestConfig controls a backtest.
type BacktestConfig struct {
	Start, End time.Time
	Step       time.Duration // tick spacing; bars of this interval are used for prices
	Interval   string        // "1h" (price bars used for fills and valuation)
	InitialUSD float64
	FeeUSD     float64 // per trade (network fee)
	CostBps    float64 // per trade, price impact + pool fees
	Rules      Rules
}

// DefaultBacktest returns a config for the last days days at hourly steps.
func DefaultBacktest(end time.Time, days int) BacktestConfig {
	rules := DefaultRules
	rules.SOLReserve = 0 // fees are charged in USD instead
	return BacktestConfig{
		Start: end.Add(-time.Duration(days) * 24 * time.Hour), End: end,
		Step: time.Hour, Interval: "1h", InitialUSD: 1000, FeeUSD: 0.0125, CostBps: 10, Rules: rules,
	}
}

// EquityPoint is the portfolio value at a time.
type EquityPoint struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// Result summarises a backtest.
type Result struct {
	Start, End  time.Time
	FinalValue  float64
	Return      float64 // fraction
	MaxDrawdown float64 // fraction
	Sharpe      float64 // annualized, from step returns
	Trades      int
	FeesUSD     float64 // network fees + trading costs
	Halted      string  // non-empty if the kill switch fired
	Equity      []EquityPoint
	Notes       []string // decision notes when they changed
}

// Backtest replays strategy s over historical bars with the live engine.
// Fills happen at the bar close, minus CostBps and FeeUSD per trade.
func Backtest(s Strategy, data *MemData, symbols []string, cfg BacktestConfig) (Result, error) {
	res := Result{Start: cfg.Start, End: cfg.End}
	units := map[string]float64{Cash: cfg.InitialUSD}
	st := &State{}
	var rets []float64
	prevValue, peak := cfg.InitialUSD, cfg.InitialUSD
	lastNote := ""

	for now := cfg.Start; !now.After(cfg.End); now = now.Add(cfg.Step) {
		data.Now = now
		pf, ok := valuePortfolio(data, cfg.Interval, units, symbols)
		if !ok {
			continue // no prices yet at this step
		}
		if res.Halted == "" {
			plan, err := Step(s, now, data, pf, st, cfg.Rules)
			if err != nil {
				return res, fmt.Errorf("%s: %w", now.Format(time.RFC3339), err)
			}
			if n := plan.Note(); n != lastNote {
				res.Notes = append(res.Notes, now.UTC().Format("2006-01-02 15:04")+" "+n)
				lastNote = n
			}
			for _, o := range plan.Orders {
				sym := o.Token()
				price := pf.Prices[sym]
				cost := cfg.CostBps / 10000
				if o.From == Cash {
					spend := math.Min(o.ValueUSD, units[Cash]-cfg.FeeUSD)
					if spend <= 0 {
						continue
					}
					got := spend * (1 - cost) / price
					units[Cash] -= spend + cfg.FeeUSD
					units[sym] += got
					st.RecordFill(sym, got, spend, units[sym])
					res.FeesUSD += spend*cost + cfg.FeeUSD
				} else {
					sell := o.ValueUSD / price
					if o.All || sell > units[sym] {
						sell = units[sym]
					}
					proceeds := sell * price
					units[sym] -= sell
					units[Cash] += proceeds*(1-cost) - cfg.FeeUSD
					st.RecordFill(sym, -sell, proceeds, units[sym])
					res.FeesUSD += proceeds*cost + cfg.FeeUSD
				}
				res.Trades++
			}
			if plan.Halt != "" {
				res.Halted = now.UTC().Format(time.RFC3339) + ": " + plan.Halt
			}
			pf, _ = valuePortfolio(data, cfg.Interval, units, symbols)
		}

		res.Equity = append(res.Equity, EquityPoint{At: now, Value: pf.Total})
		if prevValue > 0 {
			rets = append(rets, pf.Total/prevValue-1)
		}
		prevValue = pf.Total
		peak = math.Max(peak, pf.Total)
		if dd := 1 - pf.Total/peak; dd > res.MaxDrawdown {
			res.MaxDrawdown = dd
		}
	}
	if len(res.Equity) == 0 {
		return res, fmt.Errorf("no price data between %s and %s; run backfill first",
			cfg.Start.Format(time.DateOnly), cfg.End.Format(time.DateOnly))
	}
	res.FinalValue = res.Equity[len(res.Equity)-1].Value
	res.Return = res.FinalValue/cfg.InitialUSD - 1
	if sd := stdev(rets); sd > 0 {
		res.Sharpe = mean(rets) / sd * math.Sqrt(float64(365*24*time.Hour)/float64(cfg.Step))
	}
	return res, nil
}

// valuePortfolio prices holdings at the latest closed bar. ok is false if a
// held or traded token has no price yet.
func valuePortfolio(data *MemData, interval string, units map[string]float64, symbols []string) (Portfolio, bool) {
	pf := Portfolio{Values: map[string]float64{}, Units: map[string]float64{}, Prices: map[string]float64{Cash: 1}}
	for _, sym := range symbols {
		if p, ok := data.PriceAt(sym, interval); ok {
			pf.Prices[sym] = p
		} else if units[sym] > 0 || sym == "SOL" {
			return pf, false
		}
	}
	for sym, u := range units {
		pf.Units[sym] = u
		pf.Values[sym] = u * pf.Prices[sym]
		pf.Total += pf.Values[sym]
	}
	return pf, true
}

func stdev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var ss float64
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}
