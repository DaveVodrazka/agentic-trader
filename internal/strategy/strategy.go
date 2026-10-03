// Package strategy runs algorithmic trading strategies. A strategy looks at
// price history and outputs target portfolio weights; a shared engine applies
// risk rules and turns the weights into orders. The same engine drives live
// ticks and backtests, so they cannot drift apart.
//
// Strategies are spot-only and long-only: weights are fractions of the
// investable portfolio per non-USDC token, the remainder is held in USDC.
package strategy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"agentic-trader/internal/venue"
)

// Cash is the quote asset; unallocated weight is held in it.
const Cash = "USDC"

// Intervals are the candle intervals collected and used by strategies.
var Intervals = map[string]time.Duration{
	"5m": 5 * time.Minute,
	"1h": time.Hour,
	"1d": 24 * time.Hour,
}

// Data gives a strategy price history as of "now". Only closed bars are
// returned, so live and backtest decisions see the same information.
type Data interface {
	// Closes returns up to n closing prices of the latest closed bars,
	// oldest first.
	Closes(symbol, interval string, n int) []float64
}

// Portfolio is the wallet valued in USD.
type Portfolio struct {
	Total  float64
	Values map[string]float64 // symbol -> USD value
	Units  map[string]float64 // symbol -> token units
	Prices map[string]float64 // symbol -> USD per unit
}

// Weight returns symbol's share of the portfolio.
func (p Portfolio) Weight(symbol string) float64 {
	if p.Total <= 0 {
		return 0
	}
	return p.Values[symbol] / p.Total
}

// Input is what a strategy sees when deciding.
type Input struct {
	Now       time.Time
	Data      Data
	Portfolio Portfolio
	// Memory is the strategy's own persistent key/value state.
	Memory map[string]string
}

// Decision is a strategy's output.
type Decision struct {
	// Weights are target fractions of the investable portfolio per token;
	// the rest stays in USDC. Tokens not listed are targeted at zero.
	Weights map[string]float64
	// Hold means "no change this tick" (e.g. between scheduled
	// rebalances). Risk rules still apply.
	Hold bool
	// Band is the minimum drift (fraction of the portfolio) before a
	// position is traded back to target.
	Band float64
	// Note explains the decision in one line.
	Note string
}

// Strategy decides target weights.
type Strategy interface {
	Decide(in Input) (Decision, error)
}

// Spec describes a strategy for listing and construction.
type Spec struct {
	Name    string
	Summary string // what it does, when to use it
	// Defaults returns the default parameters.
	Defaults func() any
	// build validates params (already decoded onto defaults) and returns
	// the strategy.
	build func(params any, tokens *venue.TokenRegistry) (Strategy, error)
}

// Specs returns all strategies, sorted by name.
func Specs() []Spec {
	out := append([]Spec(nil), registry...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds a strategy by name.
func Lookup(name string) (Spec, error) {
	for _, s := range registry {
		if s.Name == name {
			return s, nil
		}
	}
	names := make([]string, len(registry))
	for i, s := range registry {
		names[i] = s.Name
	}
	sort.Strings(names)
	return Spec{}, fmt.Errorf("unknown strategy %q (have: %s)", name, strings.Join(names, ", "))
}

// New decodes params (JSON object; empty means defaults) onto the defaults,
// validates them and builds the strategy. It returns the effective params
// as canonical JSON for storage.
func (s Spec) New(params json.RawMessage, tokens *venue.TokenRegistry) (Strategy, json.RawMessage, error) {
	p := s.Defaults()
	if len(bytes.TrimSpace(params)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(params))
		dec.DisallowUnknownFields()
		if err := dec.Decode(p); err != nil {
			return nil, nil, fmt.Errorf("%s params: %w", s.Name, err)
		}
	}
	st, err := s.build(p, tokens)
	if err != nil {
		return nil, nil, fmt.Errorf("%s params: %w", s.Name, err)
	}
	canon, err := json.Marshal(p)
	return st, canon, err
}

// DefaultParams returns the defaults as JSON.
func (s Spec) DefaultParams() json.RawMessage {
	b, _ := json.Marshal(s.Defaults())
	return b
}

// ---- indicators ------------------------------------------------------------

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// sma is the simple moving average of the last n values.
func sma(xs []float64, n int) float64 {
	if len(xs) < n || n <= 0 {
		return math.NaN()
	}
	return mean(xs[len(xs)-n:])
}

// annualizedVol is the annualized standard deviation of log returns of
// closes sampled every period.
func annualizedVol(closes []float64, period time.Duration) float64 {
	if len(closes) < 3 {
		return math.NaN()
	}
	rets := make([]float64, 0, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		if closes[i-1] <= 0 || closes[i] <= 0 {
			return math.NaN()
		}
		rets = append(rets, math.Log(closes[i]/closes[i-1]))
	}
	m := mean(rets)
	var ss float64
	for _, r := range rets {
		ss += (r - m) * (r - m)
	}
	sd := math.Sqrt(ss / float64(len(rets)-1))
	return sd * math.Sqrt(float64(365*24*time.Hour)/float64(period))
}

// ---- param validation helpers ------------------------------------------------

func checkWeights(w map[string]float64, tokens *venue.TokenRegistry) error {
	var sum float64
	for sym, x := range w {
		if strings.EqualFold(sym, Cash) {
			return fmt.Errorf("weights: list only non-USDC tokens; the remainder is held in USDC")
		}
		if err := exactSymbol(sym, tokens); err != nil {
			return fmt.Errorf("weights: %w", err)
		}
		if x < 0 || x > 1 {
			return fmt.Errorf("weights: %s must be between 0 and 1", sym)
		}
		sum += x
	}
	if sum > 1+1e-9 {
		return fmt.Errorf("weights sum to %.3f; must be at most 1", sum)
	}
	return nil
}

func checkTokens(syms []string, tokens *venue.TokenRegistry) error {
	if len(syms) == 0 {
		return fmt.Errorf("at least one token is required")
	}
	for _, sym := range syms {
		if strings.EqualFold(sym, Cash) {
			return fmt.Errorf("USDC cannot be traded against itself")
		}
		if err := exactSymbol(sym, tokens); err != nil {
			return err
		}
	}
	return nil
}

func exactSymbol(sym string, tokens *venue.TokenRegistry) error {
	tok, err := tokens.Lookup(sym)
	if err != nil {
		return err
	}
	if tok.Symbol != sym {
		return fmt.Errorf("use %q, not %q", tok.Symbol, sym)
	}
	return nil
}

func checkRange(name string, v, lo, hi float64) error {
	if v < lo || v > hi || math.IsNaN(v) {
		return fmt.Errorf("%s must be between %g and %g", name, lo, hi)
	}
	return nil
}

func checkInterval(iv string) error {
	if _, ok := Intervals[iv]; !ok || iv == "5m" {
		return fmt.Errorf("interval must be 1h or 1d")
	}
	return nil
}
