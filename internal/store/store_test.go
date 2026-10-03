package store

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if err := s.UpsertTokens(ctx, []Token{{"USDC", "usdc-mint", 6}, {"SOL", "sol-mint", 9}}); err != nil {
		t.Fatal(err)
	}
	return s
}

var t0 = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

func TestDepositTradeBalances(t *testing.T) {
	s, ctx := open(t), context.Background()
	if ok, _ := s.Initialized(ctx); ok {
		t.Fatal("fresh db should not be initialized")
	}
	must(t, s.AddDeposit(ctx, Deposit{At: t0, Symbol: "USDC", Amount: big.NewInt(1000e6), ValueUSDC: "1000"}))
	must(t, s.AddDeposit(ctx, Deposit{At: t0, Symbol: "SOL", Amount: big.NewInt(1e8), ValueUSDC: "12"}))

	tr := &Trade{ID: "t1", RunID: "r1", At: t0.Add(time.Minute), Venue: "x", From: "USDC", To: "SOL",
		InAmount: big.NewInt(100e6), OutAmount: big.NewInt(8e8), Price: "0.008", Reason: "why",
		Fees: &TradeFees{NetworkLamports: 105000, NetworkUSDC: "0.0126"}}
	must(t, s.BeginRun(ctx, "r1", "go", t0))
	must(t, s.RecordTrade(ctx, tr))
	if tr.Seq != 1 {
		t.Errorf("seq = %d", tr.Seq)
	}
	b, _ := s.Balances(ctx)
	if b["USDC"].Int64() != 900e6 || b["SOL"].Int64() != 1e8+8e8-105000 {
		t.Errorf("balances = %v", b)
	}

	trades, _ := s.Trades(ctx)
	if len(trades) != 1 || trades[0].Fees == nil || trades[0].Fees.NetworkUSDC != "0.0126" || !trades[0].At.Equal(tr.At) {
		t.Errorf("trades = %+v", trades)
	}
}

func TestInsufficientBalanceWritesNothing(t *testing.T) {
	s, ctx := open(t), context.Background()
	must(t, s.AddDeposit(ctx, Deposit{At: t0, Symbol: "USDC", Amount: big.NewInt(100e6), ValueUSDC: "100"}))

	// Enough USDC but no SOL for the fee.
	err := s.RecordTrade(ctx, &Trade{ID: "t1", At: t0, From: "USDC", To: "SOL", InAmount: big.NewInt(10e6),
		OutAmount: big.NewInt(1), Fees: &TradeFees{NetworkLamports: 5000}})
	var be *BalanceError
	if !errors.Is(err, ErrInsufficientBalance) || !errors.As(err, &be) || be.Symbol != "SOL" {
		t.Fatalf("err = %v", err)
	}
	trades, _ := s.Trades(ctx)
	b, _ := s.Balances(ctx)
	if len(trades) != 0 || b["USDC"].Int64() != 100e6 {
		t.Errorf("state changed: trades=%d balances=%v", len(trades), b)
	}
}

func TestConcurrentTradesCannotOverdraw(t *testing.T) {
	s, ctx := open(t), context.Background()
	must(t, s.AddDeposit(ctx, Deposit{At: t0, Symbol: "USDC", Amount: big.NewInt(1000e6), ValueUSDC: "1000"}))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.RecordTrade(ctx, &Trade{ID: string(rune('a' + i)), At: t0, From: "USDC", To: "SOL",
				InAmount: big.NewInt(100e6), OutAmount: big.NewInt(1)})
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, ErrInsufficientBalance) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, _ := s.Balances(ctx)
	if ok != 10 || b["USDC"] != nil {
		t.Errorf("ok=%d balances=%v", ok, b)
	}
}

func TestJournalNarrativeAndRuns(t *testing.T) {
	s, ctx := open(t), context.Background()
	must(t, s.AddDeposit(ctx, Deposit{At: t0, Symbol: "USDC", Amount: big.NewInt(1000e6), ValueUSDC: "1000"}))
	must(t, s.BeginRun(ctx, "r1", "go", t0))
	must(t, s.RecordTrade(ctx, &Trade{ID: "t1", RunID: "r1", At: t0, From: "USDC", To: "SOL", InAmount: big.NewInt(1), OutAmount: big.NewInt(1)}))
	must(t, s.SaveNarrative(ctx, "r1", "body1", "sum1", t0.Add(time.Minute)))
	must(t, s.SaveNarrative(ctx, "r2", "body2", "sum2", t0.Add(2*time.Minute))) // run without BeginRun
	code := 0
	must(t, s.EndRun(ctx, Run{ID: "r1", StartedAt: t0, FinishedAt: t0.Add(time.Minute), ExitCode: &code, Report: "done"}))

	n, ok, err := s.LatestNarrative(ctx)
	if err != nil || !ok || n.Body != "body2" || n.RunID != "r2" {
		t.Errorf("latest = %+v ok=%v err=%v", n, ok, err)
	}
	j, _ := s.Journal(ctx, 1)
	if len(j) != 1 || j[0].Summary != "sum2" {
		t.Errorf("journal(1) = %+v", j)
	}
	j, _ = s.Journal(ctx, 0)
	if len(j) != 2 || j[0].RunID != "r1" || len(j[0].TradeIDs) != 1 || j[0].TradeIDs[0] != "t1" {
		t.Errorf("journal = %+v", j)
	}
	if n, _ := s.CountRuns(ctx); n != 2 {
		t.Errorf("runs = %d", n)
	}
}

func TestQuotesAndPrices(t *testing.T) {
	s, ctx := open(t), context.Background()
	must(t, s.RecordQuote(ctx, Quote{QuoteID: "q1", RunID: "r1", At: t0, Venue: "x", From: "USDC", To: "SOL",
		InAmount: big.NewInt(1e6), OutAmount: big.NewInt(8e6), Price: "0.008", PriceImpactPct: "0", Route: `["A"]`}))
	must(t, s.RecordPrices(ctx, []Price{{At: t0, Symbol: "SOL", USDC: "120", Source: "quote"}}))
	if id, found, err := s.QuoteTrade(ctx, "q1"); err != nil || !found || id != "" {
		t.Errorf("quote trade = %q %v %v", id, found, err)
	}
}

func snap(kind string, at time.Time, value string) *Snapshot {
	return &Snapshot{TakenAt: at, Kind: kind, Complete: true, ValueUSDC: value, DepositsUSDC: "1000", PnLUSDC: "0",
		ReturnPct: "0", GrossPnLUSDC: "0", CostsUSDC: "0", NetworkFeesSOL: "0", CashPct: "100",
		Holdings: []SnapshotHolding{
			{Symbol: "USDC", Amount: big.NewInt(500e6), PriceUSDC: "1", ValueUSDC: "500", WeightPct: "40"},
			{Symbol: "SOL", Amount: big.NewInt(6e9), PriceUSDC: "125", ValueUSDC: "750", WeightPct: "60"},
		}}
}

func TestSnapshotsNavigation(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, ok, err := s.LatestSnapshot(ctx); ok || err != nil {
		t.Fatalf("empty latest: ok=%v err=%v", ok, err)
	}
	ids := map[string]int64{}
	for i, k := range []string{KindInitial, KindRun, KindManual, KindRun, KindManual} {
		sn := snap(k, t0.Add(time.Duration(i)*time.Minute), "1000")
		must(t, s.RecordSnapshot(ctx, sn))
		ids[k+string(rune('0'+i))] = sn.ID
	}
	// ids: initial0=1 run1=2 manual2=3 run3=4 manual4=5

	latest, ok, err := s.LatestSnapshot(ctx)
	if err != nil || !ok || latest.ID != 5 || len(latest.Holdings) != 2 || latest.Holdings[0].Symbol != "SOL" {
		t.Fatalf("latest = %+v ok=%v err=%v", latest, ok, err)
	}
	if lr, _, _ := s.LatestSnapshot(ctx, KindRun); lr.ID != 4 {
		t.Errorf("latest run = %d", lr.ID)
	}

	// Stepping through manual+initial skips run snapshots.
	prev, next, err := s.AdjacentSnapshots(ctx, 5, KindManual, KindInitial)
	if err != nil || prev != 3 || next != 0 {
		t.Errorf("adjacent(5) = %d,%d err=%v", prev, next, err)
	}
	prev, next, _ = s.AdjacentSnapshots(ctx, 3, KindManual, KindInitial)
	if prev != 1 || next != 5 {
		t.Errorf("adjacent(3) = %d,%d", prev, next)
	}
	prev, next, _ = s.AdjacentSnapshots(ctx, 3)
	if prev != 2 || next != 4 {
		t.Errorf("adjacent(3, all) = %d,%d", prev, next)
	}

	page, _ := s.ListSnapshots(ctx, 0, 2)
	if len(page) != 2 || page[0].ID != 5 || page[1].ID != 4 {
		t.Errorf("page1 = %+v", page)
	}
	page, _ = s.ListSnapshots(ctx, page[1].ID, 10, KindManual, KindInitial)
	if len(page) != 2 || page[0].ID != 3 || page[1].ID != 1 || page[0].Holdings != nil {
		t.Errorf("page2 = %+v", page)
	}

	got, ok, _ := s.GetSnapshot(ctx, 1)
	if !ok || got.Kind != KindInitial || !got.TakenAt.Equal(t0) || got.Holdings[1].Amount.Int64() != 500e6 || got.APRPct != "" {
		t.Errorf("get(1) = %+v", got)
	}
	var prices int
	s.db.QueryRow(`SELECT count(*) FROM prices WHERE symbol='SOL' AND source='snapshot'`).Scan(&prices)
	if prices != 5 {
		t.Errorf("snapshot prices = %d, want 5 (USDC excluded)", prices)
	}
}

func TestSnapshotKindChecked(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.RecordSnapshot(ctx, snap("bogus", t0, "1")); err == nil {
		t.Error("invalid kind accepted")
	}
}

func TestReopenKeepsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	s, err := Open(path)
	must(t, err)
	s.Close()
	s, err = Open(path)
	must(t, err)
	defer s.Close()
	var v int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Errorf("version = %d", v)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPriceTicksBuildCandles(t *testing.T) {
	s, ctx := open(t), context.Background()
	iv := map[string]time.Duration{"1h": time.Hour, "5m": 5 * time.Minute}
	base := t0.Add(10 * time.Minute) // 10:10
	for i, p := range []float64{100, 104, 97, 101} {
		must(t, s.AddPriceTicks(ctx, base.Add(time.Duration(i)*time.Minute), map[string]float64{"SOL": p}, iv, "test"))
	}
	c, _ := s.Candles(ctx, "SOL", "1h", t0, t0.Add(time.Hour))
	if len(c) != 1 || c[0].Open != 100 || c[0].High != 104 || c[0].Low != 97 || c[0].Close != 101 || !c[0].Start.Equal(t0) {
		t.Errorf("1h = %+v", c)
	}
	c, _ = s.Candles(ctx, "SOL", "5m", t0, t0.Add(time.Hour))
	if len(c) != 1 || !c[0].Start.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("5m = %+v", c)
	}

	// Backfill replaces whole bars.
	must(t, s.UpsertCandles(ctx, []Candle{{Symbol: "SOL", Interval: "1h", Start: t0, Open: 1, High: 2, Low: 0.5, Close: 1.5, Source: "bf"}}, ReplaceAll))
	c, _ = s.Candles(ctx, "SOL", "1h", t0, t0.Add(time.Hour))
	if c[0].Close != 1.5 || c[0].Source != "bf" {
		t.Errorf("after upsert = %+v", c[0])
	}
}

func TestStrategyActivations(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, ok, _ := s.LiveStrategy(ctx); ok {
		t.Fatal("no strategy expected")
	}
	a1 := &Activation{Strategy: "cash", Params: "{}", Reason: "start", SetBy: "user", StartedAt: t0}
	must(t, s.StartStrategy(ctx, a1))
	a2 := &Activation{Strategy: "trend", Params: `{"fast":20}`, Reason: "trending", SetBy: "agent", RunID: "r1", StartedAt: t0.Add(time.Hour)}
	must(t, s.StartStrategy(ctx, a2))

	live, ok, err := s.LiveStrategy(ctx)
	if err != nil || !ok || live.ID != a2.ID || live.Status != StatusActive || live.State != "{}" {
		t.Fatalf("live = %+v ok=%v err=%v", live, ok, err)
	}
	all, _ := s.Activations(ctx, 0)
	if len(all) != 2 || all[1].Status != StatusEnded || !all[1].EndedAt.Equal(a2.StartedAt) {
		t.Errorf("activations = %+v", all)
	}

	must(t, s.SaveStrategyState(ctx, a2.ID, `{"x":1}`, ""))
	must(t, s.SaveStrategyState(ctx, a2.ID, `{"x":2}`, "drawdown"))
	live, _, _ = s.LiveStrategy(ctx)
	if live.State != `{"x":2}` || live.Status != StatusHalted || live.HaltReason != "drawdown" {
		t.Errorf("halted = %+v", live)
	}

	must(t, s.RecordTick(ctx, Tick{ActivationID: a2.ID, At: t0, ValueUSDC: "1000", Targets: "{}", Actions: "[]", Note: "n"}))
	ticks, _ := s.RecentTicks(ctx, a2.ID, 5)
	if len(ticks) != 1 || ticks[0].Note != "n" {
		t.Errorf("ticks = %+v", ticks)
	}

	must(t, s.AddDeposit(ctx, Deposit{At: t0, Symbol: "USDC", Amount: big.NewInt(100e6), ValueUSDC: "100"}))
	must(t, s.RecordTrade(ctx, &Trade{ID: "t1", At: t0, From: "USDC", To: "SOL", InAmount: big.NewInt(1e6), OutAmount: big.NewInt(1), ActivationID: a2.ID}))
	trades, _ := s.Trades(ctx)
	if trades[0].ActivationID != a2.ID {
		t.Errorf("trade activation = %d", trades[0].ActivationID)
	}
}

func TestCandleWriteModes(t *testing.T) {
	s, ctx := open(t), context.Background()
	tick := Candle{Symbol: "SOL", Interval: "1h", Start: t0, Open: 1, High: 1, Low: 1, Close: 1, Source: "jupiter"}
	gecko := Candle{Symbol: "SOL", Interval: "1h", Start: t0.Add(time.Hour), Open: 2, High: 2, Low: 2, Close: 2, Source: "gt"}
	must(t, s.UpsertCandles(ctx, []Candle{tick, gecko}, InsertMissing))

	tick2, gecko2 := tick, gecko
	tick2.Close, tick2.Source = 9, "gt"
	gecko2.Close = 3
	must(t, s.UpsertCandles(ctx, []Candle{tick2, gecko2}, ReplaceSameSource))
	cs, _ := s.Candles(ctx, "SOL", "1h", t0, t0.Add(2*time.Hour))
	if cs[0].Close != 1 || cs[0].Source != "jupiter" || cs[1].Close != 3 {
		t.Errorf("same-source = %+v", cs)
	}
	must(t, s.UpsertCandles(ctx, []Candle{tick2}, InsertMissing))
	if cs, _ := s.Candles(ctx, "SOL", "1h", t0, t0.Add(time.Hour)); cs[0].Close != 1 {
		t.Errorf("insert-missing overwrote: %+v", cs)
	}
	must(t, s.UpsertCandles(ctx, []Candle{tick2}, ReplaceAll))
	if cs, _ := s.Candles(ctx, "SOL", "1h", t0, t0.Add(time.Hour)); cs[0].Close != 9 {
		t.Errorf("replace-all = %+v", cs)
	}

	starts, _ := s.CandleStarts(ctx, "SOL", "1h", t0.Add(time.Hour))
	if len(starts) != 1 || !starts[0].Equal(t0.Add(time.Hour)) {
		t.Errorf("starts = %v", starts)
	}
	if e, ok, _ := s.EarliestCandle(ctx, "SOL", "1h"); !ok || !e.Equal(t0) {
		t.Errorf("earliest = %v %v", e, ok)
	}

	if _, ok, _ := s.GetBackfillState(ctx, "SOL", "1h"); ok {
		t.Error("unexpected state")
	}
	b := BackfillState{Symbol: "SOL", Interval: "1h", Provider: "gt", Pool: "p", PoolName: "SOL / USDC", Earliest: t0}
	must(t, s.SaveBackfillState(ctx, b, t0))
	b.CheckedThrough = t0.Add(5 * time.Hour)
	must(t, s.SaveBackfillState(ctx, b, t0))
	got, ok, _ := s.GetBackfillState(ctx, "SOL", "1h")
	if !ok || got.Pool != "p" || !got.Earliest.Equal(t0) || !got.CheckedThrough.Equal(b.CheckedThrough) {
		t.Errorf("state = %+v", got)
	}
}

func TestWakeups(t *testing.T) {
	s, ctx := open(t), context.Background()
	w := Wakeup{At: t0, Kind: "move", Key: "move:SOL", Detail: "SOL -6% in 1h"}
	if added, err := s.AddWakeup(ctx, w, time.Hour); err != nil || !added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	w.At = t0.Add(30 * time.Minute)
	if added, _ := s.AddWakeup(ctx, w, time.Hour); added {
		t.Error("debounce failed")
	}
	w.At = t0.Add(61 * time.Minute)
	if added, _ := s.AddWakeup(ctx, w, time.Hour); !added {
		t.Error("cooldown should have expired")
	}
	must(t, s.BeginRun(ctx, "r1", "", t0.Add(2*time.Hour)))
	got, err := s.ClaimWakeups(ctx, "r1", t0.Add(2*time.Hour))
	if err != nil || len(got) != 2 || got[0].Detail != "SOL -6% in 1h" {
		t.Fatalf("claimed = %+v err=%v", got, err)
	}
	if again, _ := s.ClaimWakeups(ctx, "r2", t0.Add(3*time.Hour)); len(again) != 0 {
		t.Errorf("claimed twice: %+v", again)
	}
	if at, ok, _ := s.LastRunStart(ctx); !ok || !at.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("last run = %v %v", at, ok)
	}
}
