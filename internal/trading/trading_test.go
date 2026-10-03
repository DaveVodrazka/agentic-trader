package trading

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentic-trader/internal/venue"
)

var tokens = venue.NewTokenRegistry(venue.SolanaTokens...)

func tok(t *testing.T, sym string) venue.Token {
	t.Helper()
	tk, err := tokens.Lookup(sym)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func units(t *testing.T, amount string, tk venue.Token) *big.Int {
	t.Helper()
	n, err := venue.ParseUnits(amount, tk.Decimals)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// newLedger writes a wallet with the given balances into a temp dir.
func newLedger(t *testing.T, wallet string) (*Ledger, string, string) {
	t.Helper()
	dir := t.TempDir()
	wp, tp := filepath.Join(dir, "WALLET.json"), filepath.Join(dir, "trades.jsonl")
	if err := os.WriteFile(wp, []byte(wallet), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(wp, tp, tokens)
	if err != nil {
		t.Fatal(err)
	}
	return l, wp, tp
}

func quote(t *testing.T, from, to, in, out string) *venue.Quote {
	t.Helper()
	f, o := tok(t, from), tok(t, to)
	return &venue.Quote{
		Venue: "test", From: f, To: o,
		InAmount: units(t, in, f), OutAmount: units(t, out, o), MinOutAmount: units(t, out, o),
		FetchedAt: time.Now(),
	}
}

const startWallet = `{"last_trade_seq":0,"balances":{"USDC":"1000","SOL":"0"}}`

func TestPaperExecuteUpdatesWalletAndLog(t *testing.T) {
	l, wp, tp := newLedger(t, startWallet)
	ex := NewPaperExecutor(l, DefaultLimits, noFees(t))

	fill, err := ex.Execute(context.Background(), order(quote(t, "USDC", "SOL", "100", "0.837083774")))
	if err != nil {
		t.Fatal(err)
	}
	if !fill.Paper || !strings.HasPrefix(fill.ID, "paper-") {
		t.Errorf("fill = %+v", fill)
	}
	got := l.Balances()
	if got["USDC"] != "900" || got["SOL"] != "0.837083774" {
		t.Errorf("balances = %v", got)
	}

	// Files on disk reflect the trade, and reopening yields the same state.
	if data, _ := os.ReadFile(tp); strings.Count(string(data), "\n") != 1 || !strings.Contains(string(data), `"seq":1`) {
		t.Errorf("trades file = %s", data)
	}
	l2, err := OpenLedger(wp, tp, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if b := l2.Balances(); b["USDC"] != "900" || b["SOL"] != "0.837083774" {
		t.Errorf("reopened balances = %v", b)
	}
}

func TestReplayAfterCrashBeforeSnapshot(t *testing.T) {
	l, wp, tp := newLedger(t, startWallet)
	ex := NewPaperExecutor(l, DefaultLimits, noFees(t))
	if _, err := ex.Execute(context.Background(), order(quote(t, "USDC", "SOL", "100", "1"))); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the log append but before the snapshot: restore
	// the old wallet file. Open must replay the missing trade.
	if err := os.WriteFile(wp, []byte(startWallet), 0o644); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenLedger(wp, tp, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if b := l2.Balances(); b["USDC"] != "900" || b["SOL"] != "1" {
		t.Errorf("replayed balances = %v", b)
	}
	if data, _ := os.ReadFile(wp); !strings.Contains(string(data), `"last_trade_seq": 1`) {
		t.Errorf("snapshot not rewritten: %s", data)
	}
}

func TestInsufficientBalanceWritesNothing(t *testing.T) {
	l, _, tp := newLedger(t, startWallet)
	_, err := NewPaperExecutor(l, DefaultLimits, noFees(t)).Execute(context.Background(), order(quote(t, "USDC", "SOL", "1000.01", "8")))
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if _, err := os.Stat(tp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trades file should not exist, stat err = %v", err)
	}
	if b := l.Balances(); b["USDC"] != "1000" {
		t.Errorf("balances changed: %v", b)
	}
}

func TestConcurrentExecuteCannotOverdraw(t *testing.T) {
	l, _, _ := newLedger(t, startWallet)
	ex := NewPaperExecutor(l, DefaultLimits, noFees(t))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ex.Execute(context.Background(), order(quote(t, "USDC", "SOL", "100", "1"))); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 10 || l.Balances()["USDC"] != "0" {
		t.Errorf("ok = %d, balances = %v", ok, l.Balances())
	}
}

func TestLimits(t *testing.T) {
	l, _, _ := newLedger(t, `{"balances":{"USDC":"1000","SOL":"1"}}`)
	ex := NewPaperExecutor(l, Limits{
		MaxQuoteAge: 10 * time.Second,
		MaxIn:       map[string]string{"USDC": "250"},
		ReserveSOL:  "0.05",
	}, noFees(t))
	ctx := context.Background()

	stale := quote(t, "USDC", "SOL", "10", "0.1")
	stale.FetchedAt = time.Now().Add(-time.Minute)
	if _, err := ex.Execute(ctx, order(stale)); !errors.Is(err, ErrStaleQuote) {
		t.Errorf("stale: err = %v", err)
	}
	if _, err := ex.Execute(ctx, order(quote(t, "USDC", "SOL", "300", "3"))); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("max in: err = %v", err)
	}
	if _, err := ex.Execute(ctx, order(quote(t, "SOL", "USDC", "0.96", "115"))); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("reserve: err = %v", err)
	}
	if _, err := ex.Execute(ctx, order(quote(t, "SOL", "USDC", "0.95", "114"))); err != nil {
		t.Errorf("within reserve: err = %v", err)
	}
}

func TestOpenRejectsUnknownToken(t *testing.T) {
	dir := t.TempDir()
	wp := filepath.Join(dir, "w.json")
	os.WriteFile(wp, []byte(`{"balances":{"NOPE":"1"}}`), 0o644)
	if _, err := OpenLedger(wp, filepath.Join(dir, "t.jsonl"), tokens); !errors.Is(err, venue.ErrUnknownToken) {
		t.Fatalf("err = %v, want ErrUnknownToken", err)
	}
}

// The real wallet file in the repo root must stay loadable.
func TestRepoWalletLoads(t *testing.T) {
	wp := filepath.Join("..", "..", DefaultWalletPath)
	data, err := os.ReadFile(wp)
	if err != nil {
		t.Skip(err)
	}
	l, _, _ := newLedger(t, string(data))
	if len(l.Balances()) == 0 {
		t.Error("no balances loaded")
	}
}

func order(q *venue.Quote) Order {
	return Order{Quote: q, Reason: "test", RunID: "run-test"}
}

func TestReasonRequiredAndRecorded(t *testing.T) {
	l, _, tp := newLedger(t, startWallet)
	ex := NewPaperExecutor(l, DefaultLimits, noFees(t))
	if _, err := ex.Execute(context.Background(), Order{Quote: quote(t, "USDC", "SOL", "1", "0.01"), Reason: "  "}); !errors.Is(err, ErrMissingReason) {
		t.Fatalf("err = %v, want ErrMissingReason", err)
	}
	if _, err := ex.Execute(context.Background(), Order{Quote: quote(t, "USDC", "SOL", "1", "0.01"), Reason: "range bottom", RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(tp)
	if !strings.Contains(string(data), `"run_id":"run-1"`) || !strings.Contains(string(data), `"reason":"range bottom"`) {
		t.Errorf("trade record = %s", data)
	}
}

func noFees(t *testing.T) CostEstimator {
	return CostEstimator{Fees: PaperFeeModel{}, SOL: tok(t, "SOL")}
}

// fixedPricer prices tokens at fixed USDC prices.
type fixedPricer map[string]string

func (p fixedPricer) ValueUSDC(_ context.Context, tk venue.Token, amount *big.Int) (*big.Rat, error) {
	px, ok := new(big.Rat).SetString(p[tk.Symbol])
	if !ok {
		return nil, errors.New("no price")
	}
	return new(big.Rat).Mul(px, ratUnits(amount, tk.Decimals)), nil
}

func TestNetworkFeeDeductedFromSOL(t *testing.T) {
	l, wp, tp := newLedger(t, `{"balances":{"USDC":"1000","SOL":"1"}}`)
	costs := CostEstimator{Fees: DefaultPaperFees, Pricer: fixedPricer{"SOL": "120"}, SOL: tok(t, "SOL")}
	ex := NewPaperExecutor(l, DefaultLimits, costs)

	q := quote(t, "USDC", "SOL", "100", "0.8")
	q.PriceImpact = big.NewRat(1, 100) // 1%
	fill, err := ex.Execute(context.Background(), order(q))
	if err != nil {
		t.Fatal(err)
	}
	// 1 + 0.8 - 0.000105 fee
	if b := l.Balances(); b["SOL"] != "1.799895" || b["USDC"] != "900" {
		t.Errorf("balances = %v", b)
	}
	// Fee 0.000105 SOL * 120 = 0.0126; impact on 100 USDC at 1% = 1.
	if got := fill.Costs.NetworkUSDC.FloatString(4); got != "0.0126" {
		t.Errorf("network usdc = %s", got)
	}
	if got := fill.Costs.ImpactUSDC.FloatString(4); got != "1.0000" {
		t.Errorf("impact usdc = %s", got)
	}
	data, _ := os.ReadFile(tp)
	for _, want := range []string{`"network_sol":"0.000105"`, `"network_usdc":"0.012600"`, `"price_impact_pct":"1.0000"`, `"quoted_out":"0.8"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("trade record missing %s: %s", want, data)
		}
	}

	// Replay applies the fee too.
	if err := os.WriteFile(wp, []byte(`{"balances":{"USDC":"1000","SOL":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenLedger(wp, tp, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if b := l2.Balances(); b["SOL"] != "1.799895" {
		t.Errorf("replayed SOL = %s", b["SOL"])
	}
}

func TestTradeRejectedWithoutSOLForFee(t *testing.T) {
	l, _, tp := newLedger(t, startWallet) // 0 SOL
	ex := NewPaperExecutor(l, DefaultLimits, CostEstimator{Fees: DefaultPaperFees, SOL: tok(t, "SOL")})
	_, err := ex.Execute(context.Background(), order(quote(t, "USDC", "SOL", "100", "0.8")))
	if !errors.Is(err, ErrInsufficientBalance) || !strings.Contains(err.Error(), "network fee") {
		t.Fatalf("err = %v, want insufficient SOL for network fee", err)
	}
	if _, err := os.Stat(tp); !errors.Is(err, os.ErrNotExist) {
		t.Error("trade logged despite rejection")
	}
}

func TestSellingAllSOLLeavesNothingForFee(t *testing.T) {
	l, _, _ := newLedger(t, `{"balances":{"SOL":"1"}}`)
	ex := NewPaperExecutor(l, DefaultLimits, CostEstimator{Fees: DefaultPaperFees, SOL: tok(t, "SOL")})
	if _, err := ex.Execute(context.Background(), order(quote(t, "SOL", "USDC", "1", "120"))); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if _, err := ex.Execute(context.Background(), order(quote(t, "SOL", "USDC", "0.99", "118"))); err != nil {
		t.Fatal(err)
	}
	if b := l.Balances(); b["SOL"] != "0.009895" {
		t.Errorf("SOL = %s", b["SOL"])
	}
}

func TestLegacyTradesWithoutFeesReplay(t *testing.T) {
	dir := t.TempDir()
	wp, tp := filepath.Join(dir, "w.json"), filepath.Join(dir, "t.jsonl")
	os.WriteFile(wp, []byte(`{"balances":{"USDC":"1000"}}`), 0o644)
	os.WriteFile(tp, []byte(`{"seq":1,"from":"USDC","to":"SOL","in":"100","out":"0.8"}`+"\n"), 0o644)
	l, err := OpenLedger(wp, tp, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if b := l.Balances(); b["SOL"] != "0.8" || b["USDC"] != "900" {
		t.Errorf("balances = %v", b)
	}
}

type countingPricer struct{ n int }

func (c *countingPricer) ValueUSDC(context.Context, venue.Token, *big.Int) (*big.Rat, error) {
	c.n++
	return big.NewRat(120, 1), nil
}

func TestCachedPricer(t *testing.T) {
	inner := &countingPricer{}
	p := &CachedPricer{Pricer: inner, TTL: time.Minute}
	for range 3 {
		if v, err := p.ValueUSDC(context.Background(), tok(t, "SOL"), big.NewInt(LamportsPerSOL)); err != nil || v.FloatString(0) != "120" {
			t.Fatalf("v=%v err=%v", v, err)
		}
	}
	if inner.n != 1 {
		t.Errorf("inner calls = %d, want 1", inner.n)
	}
}
