// Package pnl values the wallet in USDC and compares it with the capital
// deposited to report profit and loss.
package pnl

import (
	"context"
	"math"
	"math/big"
	"sort"
	"strconv"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

// Quote currency all values are expressed in.
const quoteSymbol = "USDC"

const year = 365 * 24 * time.Hour

// Holding is one token's current position valued in USDC.
type Holding struct {
	Symbol string
	Units  *big.Int // smallest units
	Amount string   // decimal
	Value  *big.Rat // nil if it could not be valued
	Price  *big.Rat // USDC per unit; nil if unvalued
	Weight float64  // share of valued portfolio, 0..1
	Err    error    // why valuation failed
}

// Valuation is the portfolio valued at one moment.
type Valuation struct {
	At       time.Time
	Total    *big.Rat
	Holdings []Holding // sorted by value, largest first
	Unvalued int
}

// Value prices balances (smallest units) by selling each into USDC on v,
// i.e. liquidation value after price impact.
func Value(ctx context.Context, v venue.Venue, tokens *venue.TokenRegistry, balances map[string]*big.Int, now time.Time) Valuation {
	val := Valuation{At: now, Total: new(big.Rat)}
	for sym, amt := range balances {
		if amt.Sign() == 0 {
			continue
		}
		tok, err := tokens.Lookup(sym)
		h := Holding{Symbol: sym, Units: amt, Err: err}
		if err == nil {
			h.Amount = venue.FormatUnits(amt, tok.Decimals)
			units := new(big.Rat).SetFrac(amt, pow10(tok.Decimals))
			if sym == quoteSymbol {
				h.Value = units
			} else if q, qerr := v.Quote(ctx, venue.QuoteRequest{From: sym, To: quoteSymbol, Amount: h.Amount}); qerr != nil {
				h.Err = qerr
			} else {
				h.Value = new(big.Rat).SetFrac(q.OutAmount, pow10(q.To.Decimals))
			}
			if h.Value != nil {
				h.Price = new(big.Rat).Quo(h.Value, units)
			}
		}
		if h.Value != nil {
			val.Total.Add(val.Total, h.Value)
		} else {
			val.Unvalued++
		}
		val.Holdings = append(val.Holdings, h)
	}
	total, _ := val.Total.Float64()
	for i := range val.Holdings {
		if h := &val.Holdings[i]; h.Value != nil && total > 0 {
			f, _ := h.Value.Float64()
			h.Weight = f / total
		}
	}
	sort.Slice(val.Holdings, func(i, j int) bool {
		return ratOrZero(val.Holdings[i].Value).Cmp(ratOrZero(val.Holdings[j].Value)) > 0
	})
	return val
}

// TradeStats summarises the trades.
type TradeStats struct {
	Count      int
	First      time.Time
	Last       time.Time
	VolumeUSDC *big.Rat       // sum of USDC legs (buys and sells against USDC)
	ByPair     map[string]int // "USDC->SOL" -> count
}

// CostStats sums trading costs. Net PnL already includes them; they are
// broken out to show what the strategy earned before costs.
type CostStats struct {
	NetworkSOL   *big.Rat
	NetworkUSDC  *big.Rat // valued at each trade's time
	ImpactUSDC   *big.Rat // estimate; already netted out of fills
	PlatformUSDC *big.Rat
	Tracked      int       // trades with fee data
	Untracked    int       // trades from before fee tracking
	Unpriced     int       // tracked trades with a cost that couldn't be valued
	Since        time.Time // first trade with fee data
}

// Total is the sum of all USDC-valued costs.
func (c CostStats) Total() *big.Rat {
	t := new(big.Rat).Add(c.NetworkUSDC, c.ImpactUSDC)
	return t.Add(t, c.PlatformUSDC)
}

// Report is the full PnL summary.
type Report struct {
	StartedAt    time.Time
	Elapsed      time.Duration
	InitialValue *big.Rat // sum of deposits at deposit-time value
	Deposits     []store.Deposit
	Valuation
	PnL       *big.Rat
	GrossPnL  *big.Rat // PnL before costs = PnL + Costs.Total()
	ReturnPct float64
	APR       float64 // simple annualisation
	APY       float64 // compounded annualisation
	CashPct   float64 // share held in USDC
	Trades    TradeStats
	Costs     CostStats
	Runs      int
}

// Compute builds the report from deposits, a current valuation and trades.
func Compute(deposits []store.Deposit, val Valuation, trades []store.Trade, tokens *venue.TokenRegistry) *Report {
	r := &Report{InitialValue: new(big.Rat), Deposits: deposits, Valuation: val,
		Trades: tradeStats(trades, tokens), Costs: costStats(trades)}
	for i, d := range deposits {
		if i == 0 {
			r.StartedAt = d.At
		}
		if v, ok := new(big.Rat).SetString(d.ValueUSDC); ok {
			r.InitialValue.Add(r.InitialValue, v)
		}
	}
	r.Elapsed = val.At.Sub(r.StartedAt)
	for _, h := range val.Holdings {
		if h.Symbol == quoteSymbol {
			r.CashPct = h.Weight
		}
	}

	r.PnL = new(big.Rat).Sub(val.Total, r.InitialValue)
	r.GrossPnL = new(big.Rat).Add(r.PnL, r.Costs.Total())
	if r.InitialValue.Sign() > 0 {
		ret, _ := new(big.Rat).Quo(r.PnL, r.InitialValue).Float64()
		r.ReturnPct = ret * 100
		if r.Elapsed > 0 {
			periods := float64(year) / float64(r.Elapsed)
			r.APR = ret * periods * 100
			r.APY = (math.Pow(1+ret, periods) - 1) * 100
		}
	}
	return r
}

// Snapshot converts the report into an immutable store snapshot.
func (r *Report) Snapshot(kind, runID string) *store.Snapshot {
	snap := &store.Snapshot{
		TakenAt:        r.At,
		Kind:           kind,
		RunID:          runID,
		Complete:       r.Unvalued == 0,
		ValueUSDC:      r.Total.FloatString(6),
		DepositsUSDC:   r.InitialValue.FloatString(6),
		PnLUSDC:        r.PnL.FloatString(6),
		ReturnPct:      pctString(r.ReturnPct),
		GrossPnLUSDC:   r.GrossPnL.FloatString(6),
		CostsUSDC:      r.Costs.Total().FloatString(6),
		NetworkFeesSOL: r.Costs.NetworkSOL.FloatString(9),
		CashPct:        pctString(r.CashPct * 100),
		TradeCount:     r.Trades.Count,
		RunCount:       r.Runs,
	}
	if r.Elapsed > 0 {
		snap.APRPct, snap.APYPct = pctString(r.APR), pctString(r.APY)
	}
	for _, h := range r.Holdings {
		sh := store.SnapshotHolding{Symbol: h.Symbol, Amount: h.Units}
		if h.Value != nil {
			sh.PriceUSDC = h.Price.FloatString(12)
			sh.ValueUSDC = h.Value.FloatString(6)
			sh.WeightPct = pctString(h.Weight * 100)
		} else if h.Err != nil {
			sh.Error = h.Err.Error()
		}
		snap.Holdings = append(snap.Holdings, sh)
	}
	return snap
}

// InitialSnapshot describes the starting wallet from the deposits alone,
// valued at deposit time. Needs no prices, so it is exact and offline.
func InitialSnapshot(deposits []store.Deposit, tokens *venue.TokenRegistry) *store.Snapshot {
	total := new(big.Rat)
	values := make([]*big.Rat, len(deposits))
	for i, d := range deposits {
		v, ok := new(big.Rat).SetString(d.ValueUSDC)
		if !ok {
			v = new(big.Rat)
		}
		values[i] = v
		total.Add(total, v)
	}
	snap := &store.Snapshot{Kind: store.KindInitial, Complete: true, ValueUSDC: total.FloatString(6),
		DepositsUSDC: total.FloatString(6), PnLUSDC: "0", ReturnPct: "0", GrossPnLUSDC: "0", CostsUSDC: "0",
		NetworkFeesSOL: "0", CashPct: "0"}
	bySymbol := map[string]int{}
	for i, d := range deposits {
		if i == 0 {
			snap.TakenAt = d.At
		}
		j, seen := bySymbol[d.Symbol]
		if !seen {
			j = len(snap.Holdings)
			bySymbol[d.Symbol] = j
			snap.Holdings = append(snap.Holdings, store.SnapshotHolding{Symbol: d.Symbol, Amount: new(big.Int), ValueUSDC: "0"})
		}
		h := &snap.Holdings[j]
		h.Amount = new(big.Int).Add(h.Amount, d.Amount)
		v, _ := new(big.Rat).SetString(h.ValueUSDC)
		h.ValueUSDC = v.Add(v, values[i]).FloatString(6)
	}
	for i := range snap.Holdings {
		h := &snap.Holdings[i]
		v, _ := new(big.Rat).SetString(h.ValueUSDC)
		if tok, err := tokens.Lookup(h.Symbol); err == nil && h.Amount.Sign() > 0 {
			h.PriceUSDC = new(big.Rat).Quo(v, new(big.Rat).SetFrac(h.Amount, pow10(tok.Decimals))).FloatString(12)
		}
		if total.Sign() > 0 {
			w, _ := new(big.Rat).Quo(v, total).Float64()
			h.WeightPct = pctString(w * 100)
			if h.Symbol == quoteSymbol {
				snap.CashPct = h.WeightPct
			}
		}
	}
	return snap
}

// Take values the wallet now, computes the report, stores it as a snapshot
// of the given kind and returns both.
func Take(ctx context.Context, st *store.Store, v venue.Venue, tokens *venue.TokenRegistry, kind, runID string) (*Report, *store.Snapshot, error) {
	deposits, err := st.Deposits(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(deposits) == 0 {
		return nil, nil, store.ErrNotInitialized
	}
	balances, err := st.Balances(ctx)
	if err != nil {
		return nil, nil, err
	}
	trades, err := st.Trades(ctx)
	if err != nil {
		return nil, nil, err
	}
	runs, err := st.CountRuns(ctx)
	if err != nil {
		return nil, nil, err
	}
	r := Compute(deposits, Value(ctx, v, tokens, balances, time.Now()), trades, tokens)
	r.Runs = runs
	snap := r.Snapshot(kind, runID)
	if err := st.RecordSnapshot(ctx, snap); err != nil {
		return r, nil, err
	}
	return r, snap, nil
}

// pctString formats a percentage; empty (NULL) when not finite.
func pctString(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return ""
	}
	return strconv.FormatFloat(f, 'f', 4, 64)
}

func tradeStats(trades []store.Trade, tokens *venue.TokenRegistry) TradeStats {
	s := TradeStats{VolumeUSDC: new(big.Rat), ByPair: make(map[string]int)}
	usdc, _ := tokens.Lookup(quoteSymbol)
	for _, t := range trades {
		s.Count++
		if s.First.IsZero() || t.At.Before(s.First) {
			s.First = t.At
		}
		if t.At.After(s.Last) {
			s.Last = t.At
		}
		s.ByPair[t.From+"->"+t.To]++
		switch quoteSymbol {
		case t.From:
			s.VolumeUSDC.Add(s.VolumeUSDC, new(big.Rat).SetFrac(t.InAmount, pow10(usdc.Decimals)))
		case t.To:
			s.VolumeUSDC.Add(s.VolumeUSDC, new(big.Rat).SetFrac(t.OutAmount, pow10(usdc.Decimals)))
		}
	}
	return s
}

func costStats(trades []store.Trade) CostStats {
	c := CostStats{NetworkSOL: new(big.Rat), NetworkUSDC: new(big.Rat), ImpactUSDC: new(big.Rat), PlatformUSDC: new(big.Rat)}
	for _, t := range trades {
		f := t.Fees
		if f == nil {
			c.Untracked++
			continue
		}
		c.Tracked++
		if c.Since.IsZero() || t.At.Before(c.Since) {
			c.Since = t.At
		}
		c.NetworkSOL.Add(c.NetworkSOL, big.NewRat(f.NetworkLamports, 1_000_000_000))
		priced := addRat(c.NetworkUSDC, f.NetworkUSDC)
		priced = addRat(c.ImpactUSDC, f.ImpactUSDC) && priced
		if f.PlatformFee != nil {
			priced = addRat(c.PlatformUSDC, f.PlatformUSDC) && priced
		}
		if !priced {
			c.Unpriced++
		}
	}
	return c
}

// addRat adds the decimal string s to dst, reporting whether s parsed.
func addRat(dst *big.Rat, s string) bool {
	v, ok := new(big.Rat).SetString(s)
	if ok {
		dst.Add(dst, v)
	}
	return ok
}

func ratOrZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}

func pow10(d uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil)
}
