// Package pnl values the wallet in USDC and compares it with the initial
// wallet to report profit and loss.
package pnl

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"time"

	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

// DefaultInitialWalletPath is where the starting state is recorded.
const DefaultInitialWalletPath = "INITIAL_WALLET.json"

// Quote currency all values are expressed in.
const quoteSymbol = "USDC"

const year = 365 * 24 * time.Hour

// InitialWallet is the portfolio the agent started with. ValueUSDC is fixed
// at start, since historical prices aren't available later.
type InitialWallet struct {
	StartedAt time.Time         `json:"started_at"`
	ValueUSDC string            `json:"value_usdc"`
	Balances  map[string]string `json:"balances"`
}

// ReadInitialWallet loads the initial wallet file.
func ReadInitialWallet(path string) (*InitialWallet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read initial wallet: %w", err)
	}
	var w InitialWallet
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("decode initial wallet %s: %w", path, err)
	}
	if w.StartedAt.IsZero() {
		return nil, fmt.Errorf("initial wallet %s: started_at is required", path)
	}
	if _, ok := new(big.Rat).SetString(w.ValueUSDC); !ok {
		return nil, fmt.Errorf("initial wallet %s: invalid value_usdc %q", path, w.ValueUSDC)
	}
	return &w, nil
}

// Holding is one token's current position valued in USDC.
type Holding struct {
	Symbol string
	Amount string
	Value  *big.Rat // nil if it could not be valued
	Weight float64  // share of valued portfolio, 0..1
	Err    error    // why valuation failed
}

// TradeStats summarises the trade log.
type TradeStats struct {
	Count      int
	First      time.Time
	Last       time.Time
	VolumeUSDC *big.Rat       // sum of USDC legs (buys and sells against USDC)
	ByPair     map[string]int // "USDC->SOL" -> count
}

// CostStats sums trading costs from the trade log. Net PnL already includes
// them; they are broken out to show what the strategy earned before costs.
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
	StartedAt     time.Time
	ValuedAt      time.Time
	Elapsed       time.Duration
	InitialValue  *big.Rat
	CurrentValue  *big.Rat
	PnL           *big.Rat
	ReturnPct     float64
	APR           float64 // simple annualisation
	APY           float64 // compounded annualisation
	Initial       map[string]string
	Holdings      []Holding // sorted by value, largest first
	Unvalued      int
	CashPct       float64 // share held in USDC
	Trades        TradeStats
	Costs         CostStats
	GrossPnL      *big.Rat // PnL before costs = PnL + Costs.Total()
	Runs          int
	WalletUpdated time.Time
}

// Compute values balances via v (selling each into USDC, i.e. liquidation
// value after price impact) and compares against initial.
func Compute(ctx context.Context, v venue.Venue, initial *InitialWallet, balances map[string]string,
	trades []trading.TradeRecord, now time.Time) (*Report, error) {
	initValue, _ := new(big.Rat).SetString(initial.ValueUSDC)
	r := &Report{
		StartedAt:    initial.StartedAt,
		ValuedAt:     now,
		Elapsed:      now.Sub(initial.StartedAt),
		InitialValue: initValue,
		CurrentValue: new(big.Rat),
		Initial:      initial.Balances,
		Trades:       tradeStats(trades),
		Costs:        costStats(trades),
	}

	for sym, amt := range balances {
		a, ok := new(big.Rat).SetString(amt)
		if !ok || a.Sign() == 0 {
			continue
		}
		h := Holding{Symbol: sym, Amount: amt}
		if sym == quoteSymbol {
			h.Value = a
		} else {
			q, err := v.Quote(ctx, venue.QuoteRequest{From: sym, To: quoteSymbol, Amount: amt})
			if err != nil {
				h.Err = err
				r.Unvalued++
			} else {
				h.Value = new(big.Rat).SetFrac(q.OutAmount, pow10(q.To.Decimals))
			}
		}
		if h.Value != nil {
			r.CurrentValue.Add(r.CurrentValue, h.Value)
		}
		r.Holdings = append(r.Holdings, h)
	}

	total, _ := r.CurrentValue.Float64()
	for i := range r.Holdings {
		if h := &r.Holdings[i]; h.Value != nil && total > 0 {
			f, _ := h.Value.Float64()
			h.Weight = f / total
			if h.Symbol == quoteSymbol {
				r.CashPct = h.Weight
			}
		}
	}
	sort.Slice(r.Holdings, func(i, j int) bool {
		return ratOrZero(r.Holdings[i].Value).Cmp(ratOrZero(r.Holdings[j].Value)) > 0
	})

	r.PnL = new(big.Rat).Sub(r.CurrentValue, r.InitialValue)
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
	return r, nil
}

func tradeStats(trades []trading.TradeRecord) TradeStats {
	s := TradeStats{VolumeUSDC: new(big.Rat), ByPair: make(map[string]int)}
	for _, t := range trades {
		s.Count++
		if s.First.IsZero() || t.At.Before(s.First) {
			s.First = t.At
		}
		if t.At.After(s.Last) {
			s.Last = t.At
		}
		s.ByPair[t.From+"->"+t.To]++
		var leg string
		switch quoteSymbol {
		case t.From:
			leg = t.In
		case t.To:
			leg = t.Out
		}
		if v, ok := new(big.Rat).SetString(leg); ok {
			s.VolumeUSDC.Add(s.VolumeUSDC, v)
		}
	}
	return s
}

func costStats(trades []trading.TradeRecord) CostStats {
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
		addRat(c.NetworkSOL, f.NetworkSOL)
		priced := addRat(c.NetworkUSDC, f.NetworkUSDC)
		priced = addRat(c.ImpactUSDC, f.ImpactUSDC) && priced
		if f.PlatformFee != "" {
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
