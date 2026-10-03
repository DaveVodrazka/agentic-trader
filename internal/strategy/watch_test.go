package strategy

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentic-trader/internal/store"
)

type countWaker struct{ n int }

func (w *countWaker) Wake(context.Context) error { w.n++; return nil }

func TestWakeOnMoveWithDebounceAndGap(t *testing.T) {
	now := time.Now().UTC()
	market := fixedMarket{"SOL": 94, "USDC": 1}
	r, st := newRunner(t, market, now)
	ctx := context.Background()
	// 5-minute bar from an hour ago at $100.
	must(t, st.UpsertCandles(ctx, []store.Candle{{Symbol: "SOL", Interval: "5m", Start: now.Add(-60 * time.Minute).Truncate(5 * time.Minute),
		Open: 100, High: 100, Low: 100, Close: 100, Source: "test"}}, store.ReplaceAll))

	res, err := r.Tick(ctx)
	must(t, err)
	w := &countWaker{}
	out, err := CheckWakeups(ctx, st, res, []string{"SOL"}, DefaultWakeRules, w, now)
	must(t, err)
	if len(out.Recorded) != 1 || out.Recorded[0].Key != "move:SOL" || !strings.Contains(out.Recorded[0].Detail, "-6.0%") || !out.Woke || w.n != 1 {
		t.Fatalf("out = %+v wakes=%d", out, w.n)
	}

	// Next tick: same event is debounced; the pending one is held while a
	// run started recently.
	must(t, st.BeginRun(ctx, "r1", "", now))
	out, _ = CheckWakeups(ctx, st, res, []string{"SOL"}, DefaultWakeRules, w, now.Add(5*time.Minute))
	if len(out.Recorded) != 0 || out.Woke || !strings.Contains(out.Held, "min gap") || w.n != 1 {
		t.Errorf("debounce/gap: %+v wakes=%d", out, w.n)
	}

	// The run claims it; nothing is pending afterwards.
	claimed, _ := st.ClaimWakeups(ctx, "r1", now)
	out, _ = CheckWakeups(ctx, st, res, []string{"SOL"}, DefaultWakeRules, w, now.Add(30*time.Minute))
	if len(claimed) != 1 || out.Pending != 0 || out.Woke {
		t.Errorf("after claim: claimed=%d out=%+v", len(claimed), out)
	}
}

func TestWakeOnDrawdownHaltStopAndStale(t *testing.T) {
	now := time.Now().UTC()
	r, st := newRunner(t, fixedMarket{"SOL": 100, "USDC": 1}, now)
	ctx := context.Background()
	must(t, st.RecordSnapshot(ctx, &store.Snapshot{TakenAt: now.Add(-time.Hour), Kind: store.KindRun, Complete: true,
		ValueUSDC: "1100", DepositsUSDC: "1000", PnLUSDC: "0", ReturnPct: "0", GrossPnLUSDC: "0", CostsUSDC: "0",
		NetworkFeesSOL: "0", CashPct: "0"}))
	res, err := r.Tick(ctx) // portfolio $1005 vs $1100 at last review: -8.6%
	must(t, err)
	res.Plan.Halt = "portfolio 21% below peak"
	res.Plan.NewStops = []string{"WIF"}

	out, err := CheckWakeups(ctx, st, res, []string{"SOL"}, DefaultWakeRules, nil, now)
	must(t, err)
	kinds := map[string]bool{}
	for _, w := range out.Recorded {
		kinds[w.Kind] = true
	}
	if !kinds["drawdown"] || !kinds["halt"] || !kinds["stop"] || out.Woke || !strings.Contains(out.Held, "no waker") {
		t.Errorf("out = %+v", out)
	}

	// A failed tick with prices older than 2h wakes for stale data.
	must(t, st.UpsertCandles(ctx, []store.Candle{{Symbol: "SOL", Interval: "1h", Start: now.Add(-3 * time.Hour).Truncate(time.Hour),
		Open: 1, High: 1, Low: 1, Close: 1, Source: "test"}}, store.ReplaceAll))
	later := now.Add(2 * time.Hour)
	st2 := st
	_, err = st2.ClaimWakeups(ctx, "r0", now)
	must(t, err)
	out, _ = CheckWakeups(ctx, st, nil, []string{"SOL"}, DefaultWakeRules, nil, later)
	if len(out.Recorded) != 1 || out.Recorded[0].Kind != "stale" {
		t.Errorf("stale: %+v", out)
	}
}
