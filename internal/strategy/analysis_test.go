package strategy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentic-trader/internal/store"
)

func TestSummarize(t *testing.T) {
	var cs = series("SOL", "1h", ramp(100, 0.1, 200)...)
	cs = append(cs, shift(series("SOL", "1d", ramp(80, 1, 60)...), -55*24*time.Hour)...)
	cs = append(cs, shift(series("WIF", "1d", ramp(2, -0.01, 60)...), -55*24*time.Hour)...)
	d := NewMemData(cs)
	d.Now = t0.Add(200 * time.Hour)
	stats := Summarize(d, []string{"SOL", "WIF", "BONK"})
	sol := stats[0]
	if sol.Price != "119.90" || sol.Trend1h != "up" || sol.RSI14 != "100" || sol.Change24h == "" || sol.Stale {
		t.Errorf("SOL = %+v", sol)
	}
	if sol.Trend1d != "up" || sol.RangePos30d == "" || sol.MaxDD30d == "" || sol.CorrSOL30d != "1.00" ||
		sol.Change30d == "" || sol.Vol30d == "" || strings.HasPrefix(sol.Vol30d, "+") {
		t.Errorf("SOL daily = %+v", sol)
	}
	if stats[1].Price != "" || stats[2].Symbol != "BONK" || stats[2].Price != "" {
		t.Errorf("tokens without hourly data should be empty: %+v", stats[1:])
	}
	d.Now = t0.Add(210 * time.Hour)
	if !Summarize(d, []string{"SOL"})[0].Stale {
		t.Error("expected stale")
	}
}

func TestCompareAndRunBacktest(t *testing.T) {
	var cs = series("SOL", "1h", ramp(100, 0.1, 400)...)
	cs = append(cs, series("SOL", "1d", ramp(90, 0.5, 30)...)...)
	d := NewMemData(cs)
	cfg := DefaultBacktest(t0.Add(399*time.Hour), 7)
	rows, err := Compare(d, []string{"SOL"}, cfg, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(Specs())+len(Benchmarks) || rows[0].ReturnPct < rows[len(rows)-1].ReturnPct {
		t.Errorf("rows = %+v", rows)
	}
	spec, _ := Lookup("trend")
	main, bench, err := RunBacktest(spec, json.RawMessage(`{"fast":10,"slow":30}`), d, []string{"SOL"}, cfg, tokens)
	if err != nil || len(bench) != 2 || !strings.Contains(main.Params, `"fast":10`) {
		t.Errorf("main=%+v bench=%+v err=%v", main, bench, err)
	}
}

func TestLivePerformance(t *testing.T) {
	now := time.Now().UTC()
	market := fixedMarket{"SOL": 100, "USDC": 1}
	r, st := newRunner(t, market, now)
	ctx := context.Background()
	if _, ok, _ := LivePerformance(ctx, st, []string{"SOL"}, tokens, now); ok {
		t.Fatal("no strategy expected")
	}
	_, err := Activate(ctx, st, tokens, "rebalance", nil, "test", "user", "", now, false)
	must(t, err)
	_, err = r.Tick(ctx)
	must(t, err)
	p, ok, err := LivePerformance(ctx, st, []string{"SOL"}, tokens, now)
	if err != nil || !ok || p.Strategy != "rebalance" || p.Ticks != 1 || p.Trades != 1 || p.CanSwitchIn == "" || p.StartValue == "" {
		t.Errorf("perf = %+v ok=%v err=%v", p, ok, err)
	}
}

func shift(cs []store.Candle, d time.Duration) []store.Candle {
	for i := range cs {
		cs[i].Start = cs[i].Start.Add(d)
	}
	return cs
}

func TestFmtPrice(t *testing.T) {
	for in, want := range map[float64]string{119.213: "119.21", 0.323998: "0.32400", 3.6719e-06: "0.0000036719", 2.5: "2.5000"} {
		if got := fmtPrice(in); got != want {
			t.Errorf("fmtPrice(%g) = %s, want %s", in, got, want)
		}
	}
}
