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
