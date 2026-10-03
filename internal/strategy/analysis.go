package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

// Last returns the most recent bar of symbol/interval at or before Now,
// including a bar still in progress (its close is the latest price).
func (d *MemData) Last(symbol, interval string) (start time.Time, close float64, ok bool) {
	bs := d.bars[symbol+"/"+interval]
	i := sort.Search(len(bs), func(i int) bool { return bs[i].Start.After(d.Now) })
	if i == 0 {
		return time.Time{}, 0, false
	}
	return bs[i-1].Start, bs[i-1].Close, true
}

// TokenStats summarises one token's recent market behaviour. Fields are
// formatted strings ("" when there is not enough data) so they read well
// and serialise safely.
type TokenStats struct {
	Symbol      string `json:"symbol"`
	Price       string `json:"price_usd"`
	Change24h   string `json:"change_24h_pct"`
	Change7d    string `json:"change_7d_pct"`
	Change30d   string `json:"change_30d_pct"`
	Vol30d      string `json:"volatility_30d_pct" jsonschema:"annualized volatility of daily returns"`
	Trend1h     string `json:"trend_1h" jsonschema:"up/down/mixed from MA20 vs MA50 on hourly bars"`
	Trend1d     string `json:"trend_1d" jsonschema:"up/down/mixed from MA20 vs MA50 on daily bars"`
	RSI14       string `json:"rsi14_1h" jsonschema:"<30 oversold, >70 overbought"`
	RangePos30d string `json:"range_position_30d_pct" jsonschema:"0 = 30-day low, 100 = 30-day high"`
	MaxDD30d    string `json:"max_drawdown_30d_pct"`
	CorrSOL30d  string `json:"correlation_to_sol_30d" jsonschema:"daily returns, -1..1"`
	LastBar     string `json:"last_hourly_bar"`
	Stale       bool   `json:"stale" jsonschema:"true if the newest hourly bar is over 2 hours old"`
}

// Summarize computes TokenStats for each symbol as of data.Now.
func Summarize(data *MemData, symbols []string) []TokenStats {
	solDaily := dailyReturns(data.Closes("SOL", "1d", 31))
	out := make([]TokenStats, 0, len(symbols))
	for _, sym := range symbols {
		s := TokenStats{Symbol: sym}
		start, price, ok := data.Last(sym, "1h")
		if !ok {
			out = append(out, s)
			continue
		}
		s.Price = fmtPrice(price)
		s.LastBar = start.UTC().Format(time.RFC3339)
		s.Stale = data.Now.Sub(start) > 2*time.Hour

		hourly := data.Closes(sym, "1h", 200)
		daily := data.Closes(sym, "1d", 60)
		s.Change24h = pctChange(hourly, 24, price)
		s.Change7d = pctChange(daily, 7, price)
		s.Change30d = pctChange(daily, 30, price)
		if len(daily) >= 31 {
			if v := annualizedVol(daily[len(daily)-31:], 24*time.Hour); !math.IsNaN(v) {
				s.Vol30d = fmt.Sprintf("%.1f", v*100)
			}
		}
		s.Trend1h = trendLabel(hourly, 20, 50)
		s.Trend1d = trendLabel(daily, 20, 50)
		if r := rsi(hourly, 14); !math.IsNaN(r) {
			s.RSI14 = fmt.Sprintf("%.0f", r)
		}
		if len(daily) >= 30 {
			window := append(append([]float64{}, daily[len(daily)-30:]...), price)
			lo, hi := minMax(window)
			if hi > lo {
				s.RangePos30d = fmt.Sprintf("%.0f", (price-lo)/(hi-lo)*100)
			}
			s.MaxDD30d = fmtPct(-maxDrawdown(window))
		}
		if c := correlation(dailyReturns(data.Closes(sym, "1d", 31)), solDaily); !math.IsNaN(c) {
			s.CorrSOL30d = fmt.Sprintf("%.2f", c)
		}
		out = append(out, s)
	}
	return out
}

// pctChange is the change from the close n bars back to price.
func pctChange(closes []float64, n int, price float64) string {
	if len(closes) < n || closes[len(closes)-n] <= 0 {
		return ""
	}
	return fmtPct(price/closes[len(closes)-n] - 1)
}

func trendLabel(closes []float64, fast, slow int) string {
	if len(closes) < slow {
		return ""
	}
	last, f, s := closes[len(closes)-1], sma(closes, fast), sma(closes, slow)
	switch {
	case last > s && f > s:
		return "up"
	case last < s && f < s:
		return "down"
	}
	return "mixed"
}

// rsi is the n-period relative strength index (simple averages).
func rsi(closes []float64, n int) float64 {
	if len(closes) < n+1 {
		return math.NaN()
	}
	var gain, loss float64
	for i := len(closes) - n; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	if gain+loss == 0 {
		return 50
	}
	return 100 * gain / (gain + loss)
}

func maxDrawdown(xs []float64) float64 {
	peak, dd := math.Inf(-1), 0.0
	for _, x := range xs {
		peak = math.Max(peak, x)
		if peak > 0 {
			dd = math.Max(dd, 1-x/peak)
		}
	}
	return dd
}

func dailyReturns(closes []float64) []float64 {
	var out []float64
	for i := 1; i < len(closes); i++ {
		if closes[i-1] > 0 && closes[i] > 0 {
			out = append(out, math.Log(closes[i]/closes[i-1]))
		}
	}
	return out
}

func correlation(a, b []float64) float64 {
	n := min(len(a), len(b))
	if n < 10 {
		return math.NaN()
	}
	a, b = a[len(a)-n:], b[len(b)-n:]
	ma, mb := mean(a), mean(b)
	var cov, va, vb float64
	for i := range n {
		cov += (a[i] - ma) * (b[i] - mb)
		va += (a[i] - ma) * (a[i] - ma)
		vb += (b[i] - mb) * (b[i] - mb)
	}
	if va == 0 || vb == 0 {
		return math.NaN()
	}
	return cov / math.Sqrt(va*vb)
}

func minMax(xs []float64) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, x := range xs {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	return lo, hi
}

func fmtPct(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return ""
	}
	return fmt.Sprintf("%+.1f", f*100)
}

func fmtPrice(p float64) string {
	switch {
	case p >= 100:
		return fmt.Sprintf("%.2f", p)
	case p >= 1:
		return fmt.Sprintf("%.4f", p)
	}
	if p <= 0 {
		return "0"
	}
	// Plain decimals with ~5 significant digits (no exponent): 0.0000036719.
	decimals := 4 - int(math.Floor(math.Log10(p)))
	return strconv.FormatFloat(p, 'f', decimals, 64)
}

// tokenLookup is what strategy construction needs from the token registry.
type tokenLookup = *venue.TokenRegistry

// ---- backtest comparisons ------------------------------------------------------

// Benchmarks are the reference strategies every result is compared with.
var Benchmarks = []struct{ Label, Strategy, Params string }{
	{"hold SOL", "hold", `{"weights":{"SOL":1}}`},
	{"50/50 SOL", "rebalance", `{"weights":{"SOL":0.5},"band":0.05}`},
}

// Summary is a backtest result in display form.
type Summary struct {
	Label       string   `json:"label"`
	Strategy    string   `json:"strategy"`
	Params      string   `json:"params"`
	ReturnPct   float64  `json:"return_pct"`
	MaxDDPct    float64  `json:"max_drawdown_pct"`
	Sharpe      float64  `json:"sharpe"`
	Trades      int      `json:"trades"`
	CostsUSD    float64  `json:"costs_usd"`
	Halted      string   `json:"halted,omitempty"`
	RecentNotes []string `json:"recent_decisions,omitempty"`
}

func summarize(label, name string, params json.RawMessage, r Result) Summary {
	round := func(x float64, d int) float64 { p := math.Pow(10, float64(d)); return math.Round(x*p) / p }
	return Summary{Label: label, Strategy: name, Params: string(params), ReturnPct: round(r.Return*100, 2),
		MaxDDPct: round(r.MaxDrawdown*100, 2), Sharpe: round(r.Sharpe, 2), Trades: r.Trades,
		CostsUSD: round(r.FeesUSD, 2), Halted: r.Halted}
}

// RunBacktest backtests one strategy plus the benchmarks over cfg's window.
// Benchmarks run without the weight cap, stops and kill switch.
func RunBacktest(spec Spec, params json.RawMessage, data *MemData, syms []string, cfg BacktestConfig,
	tokens tokenLookup) (Summary, []Summary, error) {
	s, canon, err := spec.New(params, tokens)
	if err != nil {
		return Summary{}, nil, err
	}
	res, err := Backtest(s, data, syms, cfg)
	if err != nil {
		return Summary{}, nil, err
	}
	main := summarize(spec.Name, spec.Name, canon, res)
	if n := len(res.Notes); n > 0 {
		main.RecentNotes = res.Notes[max(0, n-5):]
	}
	bench, err := runBenchmarks(data, syms, cfg, tokens)
	return main, bench, err
}

func runBenchmarks(data *MemData, syms []string, cfg BacktestConfig, tokens tokenLookup) ([]Summary, error) {
	bcfg := cfg
	bcfg.Rules.MaxWeight, bcfg.Rules.StopLoss, bcfg.Rules.MaxDrawdown = 1, 0, 0
	var out []Summary
	for _, b := range Benchmarks {
		spec, _ := Lookup(b.Strategy)
		s, canon, err := spec.New(json.RawMessage(b.Params), tokens)
		if err != nil {
			return nil, err
		}
		r, err := Backtest(s, data, syms, bcfg)
		if err != nil {
			return nil, err
		}
		out = append(out, summarize(b.Label, b.Strategy, canon, r))
	}
	return out, nil
}

// Compare backtests every strategy at default params, plus benchmarks,
// sorted by return.
func Compare(data *MemData, syms []string, cfg BacktestConfig, tokens tokenLookup) ([]Summary, error) {
	var out []Summary
	for _, spec := range Specs() {
		s, canon, err := spec.New(nil, tokens)
		if err != nil {
			return nil, err
		}
		r, err := Backtest(s, data, syms, cfg)
		if err != nil {
			return nil, err
		}
		out = append(out, summarize(spec.Name, spec.Name, canon, r))
	}
	bench, err := runBenchmarks(data, syms, cfg, tokens)
	if err != nil {
		return nil, err
	}
	out = append(out, bench...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ReturnPct > out[j].ReturnPct })
	return out, nil
}

// LoadHistory loads 1h and 1d candles for symbols in [from, to] into memory,
// with Now set to to.
func LoadHistory(ctx context.Context, st *store.Store, symbols []string, from, to time.Time) (*MemData, error) {
	var all []store.Candle
	for _, sym := range symbols {
		for _, iv := range []string{"1h", "1d"} {
			cs, err := st.Candles(ctx, sym, iv, from, to.Add(time.Second))
			if err != nil {
				return nil, err
			}
			all = append(all, cs...)
		}
	}
	if len(all) == 0 {
		return nil, errors.New("no price history; run 'make backfill' first")
	}
	d := NewMemData(all)
	d.Now = to
	return d, nil
}

// Performance is how the live strategy has done since activation.
type Performance struct {
	Activation   store.Activation   `json:"-"`
	Strategy     string             `json:"strategy"`
	ID           int64              `json:"activation_id"`
	Params       string             `json:"params"`
	Reason       string             `json:"reason"`
	Status       string             `json:"status"`
	HaltReason   string             `json:"halt_reason,omitempty"`
	Since        time.Time          `json:"since"`
	RunningFor   string             `json:"running_for"`
	CanSwitchIn  string             `json:"can_switch_in,omitempty" jsonschema:"time left before set_strategy is allowed (minimum hold)"`
	Ticks        int                `json:"ticks"`
	StartValue   string             `json:"start_value_usd,omitempty"`
	CurrentValue string             `json:"current_value_usd,omitempty"`
	ReturnPct    string             `json:"return_pct,omitempty"`
	Trades       int                `json:"trades"`
	CostsUSD     string             `json:"costs_usd"`
	Benchmarks   []Summary          `json:"benchmarks_same_window,omitempty"`
	LastNote     string             `json:"last_decision,omitempty"`
	Entries      map[string]float64 `json:"entry_prices,omitempty"`
	Stopped      []string           `json:"stopped_out,omitempty"`
}

// LivePerformance reports on the live activation; ok is false if none.
func LivePerformance(ctx context.Context, st *store.Store, symbols []string, tokens tokenLookup, now time.Time) (Performance, bool, error) {
	a, ok, err := st.LiveStrategy(ctx)
	if err != nil || !ok {
		return Performance{}, false, err
	}
	p := Performance{Activation: a, Strategy: a.Strategy, ID: a.ID, Params: a.Params, Reason: a.Reason, Status: a.Status,
		HaltReason: a.HaltReason, Since: a.StartedAt, RunningFor: now.Sub(a.StartedAt).Round(time.Minute).String()}
	if left := MinHold - now.Sub(a.StartedAt); left > 0 && a.Status == store.StatusActive {
		p.CanSwitchIn = left.Round(time.Minute).String()
	}
	var state State
	if json.Unmarshal([]byte(a.State), &state) == nil {
		p.Entries, p.Stopped = state.Entry, state.Stopped
	}

	first, last, n, err := st.TickRange(ctx, a.ID)
	if err != nil {
		return p, true, err
	}
	p.Ticks = n
	if n > 0 {
		p.StartValue, p.CurrentValue, p.LastNote = first.ValueUSDC, last.ValueUSDC, last.Note
		var v0, v1 float64
		fmt.Sscanf(first.ValueUSDC, "%g", &v0)
		fmt.Sscanf(last.ValueUSDC, "%g", &v1)
		if v0 > 0 {
			p.ReturnPct = fmtPct(v1/v0 - 1)
		}
	}

	trades, err := st.Trades(ctx)
	if err != nil {
		return p, true, err
	}
	var costs float64
	for _, t := range trades {
		if t.ActivationID != a.ID {
			continue
		}
		p.Trades++
		if t.Fees != nil {
			for _, v := range []string{t.Fees.NetworkUSDC, t.Fees.ImpactUSDC, t.Fees.PlatformUSDC} {
				var f float64
				fmt.Sscanf(v, "%g", &f)
				costs += f
			}
		}
	}
	p.CostsUSD = fmt.Sprintf("%.2f", costs)

	// Benchmarks over the same window, once there is at least an hour.
	start := a.StartedAt.Truncate(time.Hour).Add(time.Hour)
	if now.Sub(start) >= time.Hour {
		data, err := LoadHistory(ctx, st, symbols, start.Add(-time.Hour), now)
		if err == nil {
			cfg := DefaultBacktest(now.Truncate(time.Hour), 0)
			cfg.Start = start
			p.Benchmarks, _ = runBenchmarks(data, symbols, cfg, tokens)
		}
	}
	return p, true, nil
}
