package strategy

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

var tokens = venue.NewTokenRegistry(venue.SolanaTokens...)

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// series builds bars for sym from closes, one per interval starting at t0.
func series(sym, interval string, closes ...float64) []store.Candle {
	d := Intervals[interval]
	out := make([]store.Candle, len(closes))
	for i, c := range closes {
		out[i] = store.Candle{Symbol: sym, Interval: interval, Start: t0.Add(time.Duration(i) * d), Open: c, High: c, Low: c, Close: c}
	}
	return out
}

func ramp(from, step float64, n int) []float64 {
	xs := make([]float64, n)
	for i := range xs {
		xs[i] = from + step*float64(i)
	}
	return xs
}

func build(t *testing.T, name, params string) Strategy {
	t.Helper()
	spec, err := Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := spec.New(json.RawMessage(params), tokens)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func cashPortfolio(usd float64, solPrice float64) Portfolio {
	return Portfolio{Total: usd, Values: map[string]float64{Cash: usd}, Units: map[string]float64{Cash: usd},
		Prices: map[string]float64{Cash: 1, "SOL": solPrice}}
}

func TestMemDataOnlyClosedBars(t *testing.T) {
	d := NewMemData(series("SOL", "1h", 1, 2, 3, 4))
	d.Now = t0.Add(2*time.Hour + 30*time.Minute) // bars 0,1 closed; bar 2 open
	if got := d.Closes("SOL", "1h", 10); len(got) != 2 || got[1] != 2 {
		t.Errorf("closes = %v", got)
	}
	d.Now = t0.Add(3 * time.Hour) // bar 2 just closed
	if got := d.Closes("SOL", "1h", 2); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("closes = %v", got)
	}
}

func TestParamsValidation(t *testing.T) {
	cases := map[string]string{
		"trend":     `{"weights":{"SOL":0.5},"fast":50,"slow":20}`,
		"rebalance": `{"weights":{"SOL":0.7,"JUP":0.5}}`,
		"hold":      `{"weights":{"USDC":1}}`,
		"momentum":  `{"n":9}`,
		"voltarget": `{"token":"sol"}`,
		"cash":      `{"x":1}`,
	}
	for name, params := range cases {
		spec, _ := Lookup(name)
		if _, _, err := spec.New(json.RawMessage(params), tokens); err == nil {
			t.Errorf("%s %s: expected error", name, params)
		}
	}
	spec, _ := Lookup("trend")
	_, canon, err := spec.New(json.RawMessage(`{"fast":10}`), tokens)
	if err != nil || !strings.Contains(string(canon), `"fast":10`) || !strings.Contains(string(canon), `"slow":50`) {
		t.Errorf("canonical params = %s err=%v", canon, err)
	}
	if _, err := Lookup("nope"); err == nil {
		t.Error("unknown strategy accepted")
	}
}

func TestTrendSignals(t *testing.T) {
	s := build(t, "trend", `{"weights":{"SOL":0.6},"fast":3,"slow":5,"interval":"1h"}`)
	up := NewMemData(series("SOL", "1h", ramp(100, 1, 10)...))
	up.Now = t0.Add(10 * time.Hour)
	dec, _ := s.Decide(Input{Now: up.Now, Data: up, Memory: map[string]string{}})
	if dec.Weights["SOL"] != 0.6 {
		t.Errorf("uptrend: %+v", dec)
	}
	down := NewMemData(series("SOL", "1h", ramp(110, -1, 10)...))
	down.Now = t0.Add(10 * time.Hour)
	dec, _ = s.Decide(Input{Now: down.Now, Data: down, Memory: map[string]string{}})
	if dec.Weights["SOL"] != 0 {
		t.Errorf("downtrend: %+v", dec)
	}
	short := NewMemData(series("SOL", "1h", 1, 2))
	short.Now = t0.Add(2 * time.Hour)
	dec, _ = s.Decide(Input{Now: short.Now, Data: short, Memory: map[string]string{}})
	if len(dec.Weights) != 0 || !strings.Contains(dec.Note, "2/5 bars") {
		t.Errorf("insufficient data: %+v", dec)
	}
}

func TestMomentumPicksLeadersAndWaits(t *testing.T) {
	s := build(t, "momentum", `{"universe":["SOL","JUP","WIF"],"lookback_days":3,"n":2,"rebalance_hours":24,"max_weight":0.4}`)
	var cs []store.Candle
	cs = append(cs, series("SOL", "1d", 100, 101, 102, 103)...) // +3%
	cs = append(cs, series("JUP", "1d", 1, 1.2, 1.3, 1.5)...)   // +50%
	cs = append(cs, series("WIF", "1d", 1, 0.9, 0.8, 0.7)...)   // -30%
	d := NewMemData(cs)
	d.Now = t0.Add(4 * 24 * time.Hour)
	mem := map[string]string{}
	dec, _ := s.Decide(Input{Now: d.Now, Data: d, Memory: mem})
	if dec.Weights["JUP"] != 0.4 || dec.Weights["SOL"] != 0.4 || dec.Weights["WIF"] != 0 || dec.Hold {
		t.Errorf("decision = %+v", dec)
	}
	dec, _ = s.Decide(Input{Now: d.Now.Add(time.Hour), Data: d, Memory: mem})
	if !dec.Hold {
		t.Errorf("should hold until next rebalance: %+v", dec)
	}
}

func TestVolTarget(t *testing.T) {
	s := build(t, "voltarget", `{"token":"SOL","target_vol":0.4,"window_days":5,"max_weight":0.8,"band":0.05}`)
	// Alternating ±1% daily moves: vol ≈ 0.02*sqrt(365) ≈ 38% -> weight ≈ 0.4/0.38 capped 0.8.
	calm := NewMemData(series("SOL", "1d", 100, 101, 100, 101, 100, 101))
	calm.Now = t0.Add(6 * 24 * time.Hour)
	dec, _ := s.Decide(Input{Now: calm.Now, Data: calm, Memory: map[string]string{}})
	if math.Abs(dec.Weights["SOL"]-0.8) > 1e-9 {
		t.Errorf("calm: %+v", dec)
	}
	// ±9.5% daily log moves: sd ≈ 0.104, vol ≈ 0.104*sqrt(365) ≈ 1.99 -> weight ≈ 0.20.
	wild := NewMemData(series("SOL", "1d", 100, 110, 100, 110, 100, 110))
	wild.Now = calm.Now
	dec, _ = s.Decide(Input{Now: wild.Now, Data: wild, Memory: map[string]string{}})
	if w := dec.Weights["SOL"]; w < 0.19 || w > 0.21 {
		t.Errorf("wild: %+v", dec)
	}
}

func TestStepOrdersAndRules(t *testing.T) {
	rules := DefaultRules
	s := build(t, "rebalance", `{"weights":{"SOL":0.5},"band":0.05}`)
	st := &State{}
	pf := cashPortfolio(1000, 100)
	plan, err := Step(s, t0, NewMemData(nil), pf, st, rules)
	if err != nil {
		t.Fatal(err)
	}
	// Reserve 0.05 SOL = $5; investable $995; SOL target 497.5 + 5.
	if len(plan.Orders) != 1 || plan.Orders[0].To != "SOL" || math.Abs(plan.Orders[0].ValueUSD-502.5) > 1e-9 {
		t.Fatalf("orders = %+v", plan.Orders)
	}

	// Within band: no trade.
	pf = Portfolio{Total: 1000, Values: map[string]float64{Cash: 480, "SOL": 520}, Prices: map[string]float64{Cash: 1, "SOL": 100}}
	if plan, _ := Step(s, t0, NewMemData(nil), pf, st, rules); len(plan.Orders) != 0 {
		t.Errorf("within band, orders = %+v", plan.Orders)
	}

	// Max weight cap.
	big := build(t, "hold", `{"weights":{"SOL":0.9}}`)
	plan, _ = Step(big, t0, NewMemData(nil), cashPortfolio(1000, 100), &State{}, rules)
	if plan.Targets["SOL"] != 0.6 || !strings.Contains(plan.Note(), "cap SOL") {
		t.Errorf("cap: %+v", plan)
	}
}

func TestStopLossAndDrawdown(t *testing.T) {
	rules := DefaultRules
	s := build(t, "rebalance", `{"weights":{"SOL":0.5,"WIF":0.3},"band":0.05}`)
	st := &State{Entry: map[string]float64{"WIF": 1.0}, Peak: 1000}
	pf := Portfolio{Total: 950, Values: map[string]float64{Cash: 400, "SOL": 400, "WIF": 150},
		Prices: map[string]float64{Cash: 1, "SOL": 100, "WIF": 0.8}}
	plan, _ := Step(s, t0, NewMemData(nil), pf, st, rules)
	var sold bool
	for _, o := range plan.Orders {
		if o.From == "WIF" && o.All {
			sold = true
		}
	}
	if !sold || len(st.Stopped) != 1 || plan.Targets["WIF"] != 0 {
		t.Errorf("stop not applied: %+v state=%+v", plan, st)
	}

	pf.Total = 790 // 21% below the 1000 peak
	plan, _ = Step(s, t0, NewMemData(nil), pf, st, rules)
	if plan.Halt == "" || len(plan.Targets) != 0 {
		t.Errorf("drawdown halt missing: %+v", plan)
	}
}

func TestBacktestBuyAndHoldMatchesPrice(t *testing.T) {
	closes := ramp(100, 1, 200) // +1/hour
	d := NewMemData(series("SOL", "1h", closes...))
	cfg := DefaultBacktest(t0.Add(199*time.Hour), 7)
	cfg.Start = t0.Add(time.Hour)
	cfg.CostBps, cfg.FeeUSD = 0, 0
	cfg.Rules.MaxWeight = 1

	res, err := Backtest(build(t, "hold", `{"weights":{"SOL":1}}`), d, []string{"SOL"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Bought at the first closed bar (100), valued at the last closed (298).
	if math.Abs(res.Return-(298.0/100-1)) > 1e-9 || res.Trades != 1 || res.MaxDrawdown != 0 {
		t.Errorf("result = %+v", res)
	}

	cash, _ := Backtest(build(t, "cash", ``), d, []string{"SOL"}, cfg)
	if cash.Return != 0 || cash.Trades != 0 {
		t.Errorf("cash = %+v", cash)
	}

	cfg.CostBps, cfg.FeeUSD = 10, 1
	costly, _ := Backtest(build(t, "hold", `{"weights":{"SOL":1}}`), d, []string{"SOL"}, cfg)
	if costly.Return >= res.Return || costly.FeesUSD < 1 {
		t.Errorf("costs not applied: %+v", costly)
	}
}

func TestBacktestNeedsData(t *testing.T) {
	_, err := Backtest(build(t, "cash", ``), NewMemData(nil), []string{"SOL"}, DefaultBacktest(t0, 7))
	if err == nil || !strings.Contains(err.Error(), "backfill") {
		t.Errorf("err = %v", err)
	}
}

// Regression: tokens held before a strategy took over used to count as
// bought at $0, so a later buy averaged the entry far too low and the
// stop-loss could never fire.
func TestInheritedHoldingsEntryPrice(t *testing.T) {
	s := build(t, "rebalance", `{"weights":{"SOL":0.6}}`)
	st := &State{}
	pf := Portfolio{Total: 1000, Values: map[string]float64{Cash: 600, "SOL": 400}, Units: map[string]float64{"SOL": 3.35},
		Prices: map[string]float64{Cash: 1, "SOL": 119.4}}
	plan, err := Step(s, t0, NewMemData(nil), pf, st, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if st.Entry["SOL"] != 119.4 || !strings.Contains(plan.Note(), "entry at market for held SOL") {
		t.Fatalf("entry = %v note=%q", st.Entry, plan.Note())
	}
	// The strategy buys 1.69 SOL at $119.61.
	st.RecordFill("SOL", 1.6933, 202.54, 5.0437)
	if e := st.Entry["SOL"]; e < 119.4 || e > 119.62 {
		t.Errorf("entry after buy = %v, want ~119.5", e)
	}
	// And RecordFill alone never values unknown held units at zero.
	fresh := &State{}
	fresh.RecordFill("SOL", 1.6933, 202.54, 5.0437)
	if e := fresh.Entry["SOL"]; e < 119.6 || e > 119.62 {
		t.Errorf("fresh entry = %v, want 119.61", e)
	}
	// A 20% drop now trips the stop.
	pf.Prices["SOL"] = 95
	pf.Values["SOL"] = 5.0437 * 95
	plan, _ = Step(s, t0, NewMemData(nil), pf, st, DefaultRules)
	if len(plan.NewStops) != 1 || plan.NewStops[0] != "SOL" {
		t.Errorf("stop not triggered: %+v", plan)
	}
}
