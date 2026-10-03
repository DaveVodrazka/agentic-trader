package strategy

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Rules are safety limits applied on top of every strategy.
type Rules struct {
	MinTradeUSD  float64 // skip trades smaller than this...
	MinTradeFrac float64 // ...or than this fraction of the portfolio
	MaxWeight    float64 // max weight of any one non-USDC token
	SOLReserve   float64 // SOL units always kept for network fees
	StopLoss     float64 // exit a token that falls this fraction below its entry price
	MaxDrawdown  float64 // halt (all cash) when the portfolio falls this fraction below its peak
}

// DefaultRules are the live defaults.
var DefaultRules = Rules{
	MinTradeUSD:  10,
	MinTradeFrac: 0.02,
	MaxWeight:    0.6,
	SOLReserve:   0.05,
	StopLoss:     0.15,
	MaxDrawdown:  0.20,
}

// State is everything the engine remembers about an activation between
// ticks. It is persisted as JSON.
type State struct {
	Memory  map[string]string  `json:"memory"`  // the strategy's own state
	Entry   map[string]float64 `json:"entry"`   // average entry price per token
	Peak    float64            `json:"peak"`    // highest portfolio value seen
	Stopped []string           `json:"stopped"` // tokens exited by stop-loss; not re-bought
}

func (s *State) init() {
	if s.Memory == nil {
		s.Memory = map[string]string{}
	}
	if s.Entry == nil {
		s.Entry = map[string]float64{}
	}
}

func (s *State) stopped(sym string) bool {
	for _, x := range s.Stopped {
		if x == sym {
			return true
		}
	}
	return false
}

// RecordFill updates the average entry price after a fill. units is the
// change in token units (negative for sells), usd the value traded.
func (s *State) RecordFill(sym string, units, usd, newUnits float64) {
	s.init()
	if newUnits <= 1e-12 {
		delete(s.Entry, sym)
		return
	}
	if units > 0 {
		oldUnits := newUnits - units
		s.Entry[sym] = (s.Entry[sym]*math.Max(oldUnits, 0) + usd) / newUnits
	}
}

// Order is a trade the engine wants. Sells are token -> USDC, buys USDC ->
// token. ValueUSD is the size; All means sell the entire tradable balance.
type Order struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	ValueUSD float64 `json:"value_usd"`
	All      bool    `json:"all,omitempty"`
}

// Token is the non-USDC side of the order.
func (o Order) Token() string {
	if o.From == Cash {
		return o.To
	}
	return o.From
}

// Plan is the engine's output for one tick.
type Plan struct {
	Decision Decision
	Targets  map[string]float64 // final weights after risk rules
	Orders   []Order            // sells first, then buys
	Halt     string             // non-empty: the activation must halt
	NewStops []string           // tokens stopped out this tick
	Events   []string           // risk events (stops, caps, halts)
}

// Note summarises the plan in one line.
func (p Plan) Note() string {
	parts := []string{p.Decision.Note}
	parts = append(parts, p.Events...)
	return strings.Join(nonEmpty(parts), " | ")
}

// Step runs one tick: risk checks, the strategy's decision, and orders to
// reach the targets. It mutates st (peak, stops, strategy memory); entry
// prices are updated by the caller via RecordFill as orders fill.
func Step(s Strategy, now time.Time, data Data, pf Portfolio, st *State, rules Rules) (Plan, error) {
	st.init()
	var plan Plan

	// Drawdown kill switch.
	if pf.Total > st.Peak {
		st.Peak = pf.Total
	}
	if rules.MaxDrawdown > 0 && st.Peak > 0 && pf.Total < st.Peak*(1-rules.MaxDrawdown) {
		plan.Halt = fmt.Sprintf("portfolio $%.2f is %.1f%% below peak $%.2f (limit %.0f%%)",
			pf.Total, (1-pf.Total/st.Peak)*100, st.Peak, rules.MaxDrawdown*100)
		plan.Events = append(plan.Events, "HALT: "+plan.Halt)
		plan.Decision = Decision{Weights: map[string]float64{}, Note: "halted, moving to cash"}
		plan.Targets = map[string]float64{}
		plan.Orders = orders(pf, plan.Targets, 0, rules)
		return plan, nil
	}

	// Per-position stop-losses.
	newStops := false
	for _, sym := range sortedKeys(st.Entry) {
		entry, price := st.Entry[sym], pf.Prices[sym]
		if rules.StopLoss > 0 && entry > 0 && price > 0 && price < entry*(1-rules.StopLoss) && !st.stopped(sym) {
			st.Stopped = append(st.Stopped, sym)
			plan.NewStops = append(plan.NewStops, sym)
			newStops = true
			plan.Events = append(plan.Events, fmt.Sprintf("STOP %s: $%.4g is %.1f%% below entry $%.4g", sym, price, (1-price/entry)*100, entry))
		}
	}

	in := Input{Now: now, Data: data, Portfolio: pf, Memory: st.Memory}
	dec, err := s.Decide(in)
	if err != nil {
		return plan, err
	}
	plan.Decision = dec
	if dec.Hold && !newStops {
		return plan, nil
	}

	targets := map[string]float64{}
	if dec.Hold {
		// Keep current weights, but exit newly stopped tokens.
		for sym, v := range pf.Values {
			if sym != Cash && v > 0 {
				targets[sym] = v / pf.Total
			}
		}
	} else {
		for sym, w := range dec.Weights {
			targets[sym] = w
		}
	}
	for _, sym := range st.Stopped {
		if targets[sym] > 0 {
			delete(targets, sym)
		}
	}
	var sum float64
	for _, sym := range sortedKeys(targets) {
		if rules.MaxWeight > 0 && targets[sym] > rules.MaxWeight {
			plan.Events = append(plan.Events, fmt.Sprintf("cap %s %.0f%% -> %.0f%%", sym, targets[sym]*100, rules.MaxWeight*100))
			targets[sym] = rules.MaxWeight
		}
		sum += targets[sym]
	}
	if sum > 1 {
		for sym := range targets {
			targets[sym] /= sum
		}
	}
	plan.Targets = targets
	plan.Orders = orders(pf, targets, dec.Band, rules)
	return plan, nil
}

// orders computes the trades from the current portfolio to target weights.
// The SOL fee reserve is set aside before weights are applied.
func orders(pf Portfolio, targets map[string]float64, band float64, rules Rules) []Order {
	if pf.Total <= 0 {
		return nil
	}
	reserveUSD := rules.SOLReserve * pf.Prices["SOL"]
	investable := math.Max(pf.Total-reserveUSD, 0)
	threshold := math.Max(rules.MinTradeUSD, math.Max(rules.MinTradeFrac, band)*pf.Total)

	syms := map[string]bool{}
	for sym, v := range pf.Values {
		if sym != Cash && v > 0 {
			syms[sym] = true
		}
	}
	for sym := range targets {
		syms[sym] = true
	}
	keys := make([]string, 0, len(syms))
	for k := range syms {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sells, buys []Order
	for _, sym := range keys {
		if pf.Prices[sym] <= 0 {
			continue // can't size or fill without a price
		}
		target := targets[sym] * investable
		if sym == "SOL" {
			target += reserveUSD
		}
		cur := pf.Values[sym]
		diff := target - cur
		switch {
		case targets[sym] == 0 && sym != "SOL" && cur >= rules.MinTradeUSD:
			// Exit fully (no dust left behind).
			sells = append(sells, Order{From: sym, To: Cash, ValueUSD: cur, All: true})
		case diff <= -threshold:
			sells = append(sells, Order{From: sym, To: Cash, ValueUSD: -diff})
		case diff >= threshold:
			buys = append(buys, Order{From: Cash, To: sym, ValueUSD: diff})
		}
	}

	// Buys are limited by the USDC available after sells.
	cashAvail := pf.Values[Cash]
	for _, o := range sells {
		cashAvail += o.ValueUSD
	}
	var want float64
	for _, o := range buys {
		want += o.ValueUSD
	}
	if want > cashAvail && want > 0 {
		scale := cashAvail / want
		kept := buys[:0]
		for _, o := range buys {
			o.ValueUSD *= scale
			if o.ValueUSD >= rules.MinTradeUSD {
				kept = append(kept, o)
			}
		}
		buys = kept
	}
	return append(sells, buys...)
}

func nonEmpty(xs []string) []string {
	out := xs[:0]
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}
