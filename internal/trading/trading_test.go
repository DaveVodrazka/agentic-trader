package trading

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentic-trader/internal/store"
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

// newLedger opens a temp database funded with the given deposits, e.g.
// map{"USDC": "1000"}.
func newLedger(t *testing.T, deposits map[string]string) (*Ledger, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	var toks []store.Token
	for _, tk := range venue.SolanaTokens {
		toks = append(toks, store.Token{Symbol: tk.Symbol, Mint: tk.Address, Decimals: tk.Decimals})
	}
	if err := st.UpsertTokens(ctx, toks); err != nil {
		t.Fatal(err)
	}
	for sym, amt := range deposits {
		if err := st.AddDeposit(ctx, store.Deposit{At: time.Now(), Symbol: sym, Amount: units(t, amt, tok(t, sym)), ValueUSDC: "0"}); err != nil {
			t.Fatal(err)
		}
	}
	return NewLedger(st, tokens), st
}

func balances(t *testing.T, l *Ledger) map[string]string {
	t.Helper()
	b, err := l.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return b
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

func order(q *venue.Quote) Order {
	return Order{Quote: q, Reason: "test", RunID: "run-test"}
}

func noFees(t *testing.T) CostEstimator {
	return CostEstimator{Fees: PaperFeeModel{}, SOL: tok(t, "SOL")}
}

var usdc1000 = map[string]string{"USDC": "1000"}

func TestPaperExecuteUpdatesBalancesAndTrades(t *testing.T) {
	l, st := newLedger(t, usdc1000)
	ex := NewPaperExecutor(l, DefaultLimits, noFees(t))

	fill, err := ex.Execute(context.Background(), order(quote(t, "USDC", "SOL", "100", "0.837083774")))
	if err != nil {
		t.Fatal(err)
	}
	if !fill.Paper || !strings.HasPrefix(fill.ID, "paper-") {
		t.Errorf("fill = %+v", fill)
	}
	if b := balances(t, l); b["USDC"] != "900" || b["SOL"] != "0.837083774" {
		t.Errorf("balances = %v", b)
	}
	trades, _ := st.Trades(context.Background())
	if len(trades) != 1 || trades[0].Seq != 1 || trades[0].Reason != "test" || trades[0].RunID != "run-test" {
		t.Errorf("trades = %+v", trades)
	}
}

func TestInsufficientBalanceWritesNothing(t *testing.T) {
	l, st := newLedger(t, usdc1000)
	_, err := NewPaperExecutor(l, DefaultLimits, noFees(t)).Execute(context.Background(), order(quote(t, "USDC", "SOL", "1000.01", "8")))
	if !errors.Is(err, ErrInsufficientBalance) || !strings.Contains(err.Error(), "need 1000.01 USDC, have 1000") {
		t.Fatalf("err = %v", err)
	}
	if trades, _ := st.Trades(context.Background()); len(trades) != 0 {
		t.Error("trade written despite rejection")
	}
	if b := balances(t, l); b["USDC"] != "1000" {
		t.Errorf("balances changed: %v", b)
	}
}

func TestConcurrentExecuteCannotOverdraw(t *testing.T) {
	l, _ := newLedger(t, usdc1000)
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
	if b := balances(t, l); ok != 10 || b["USDC"] != "" {
		t.Errorf("ok = %d, balances = %v", ok, b)
	}
}

func TestLimits(t *testing.T) {
	l, _ := newLedger(t, map[string]string{"USDC": "1000", "SOL": "1"})
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

func TestReasonRequired(t *testing.T) {
	l, _ := newLedger(t, usdc1000)
	ex := NewPaperExecutor(l, DefaultLimits, noFees(t))
	if _, err := ex.Execute(context.Background(), Order{Quote: quote(t, "USDC", "SOL", "1", "0.01"), Reason: "  "}); !errors.Is(err, ErrMissingReason) {
		t.Fatalf("err = %v, want ErrMissingReason", err)
	}
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

func TestNetworkFeeDeductedAndRecorded(t *testing.T) {
	l, st := newLedger(t, map[string]string{"USDC": "1000", "SOL": "1"})
	costs := CostEstimator{Fees: DefaultPaperFees, Pricer: fixedPricer{"SOL": "120"}, SOL: tok(t, "SOL")}
	ex := NewPaperExecutor(l, DefaultLimits, costs)

	q := quote(t, "USDC", "SOL", "100", "0.8")
	q.PriceImpact = big.NewRat(1, 100) // 1%
	fill, err := ex.Execute(context.Background(), order(q))
	if err != nil {
		t.Fatal(err)
	}
	// 1 + 0.8 - 0.000105 fee
	if b := balances(t, l); b["SOL"] != "1.799895" || b["USDC"] != "900" {
		t.Errorf("balances = %v", b)
	}
	// Fee 0.000105 SOL * 120 = 0.0126; impact on 100 USDC at 1% = 1.
	if got := fill.Costs.NetworkUSDC.FloatString(4); got != "0.0126" {
		t.Errorf("network usdc = %s", got)
	}
	if got := fill.Costs.ImpactUSDC.FloatString(4); got != "1.0000" {
		t.Errorf("impact usdc = %s", got)
	}
	trades, _ := st.Trades(context.Background())
	f := trades[0].Fees
	if f == nil || f.NetworkLamports != 105000 || f.NetworkUSDC != "0.012600" || f.PriceImpactPct != "1.0000" || trades[0].QuotedOut.String() != "800000000" {
		t.Errorf("trade = %+v fees = %+v", trades[0], f)
	}
}

func TestTradeRejectedWithoutSOLForFee(t *testing.T) {
	l, _ := newLedger(t, usdc1000) // 0 SOL
	ex := NewPaperExecutor(l, DefaultLimits, CostEstimator{Fees: DefaultPaperFees, SOL: tok(t, "SOL")})
	_, err := ex.Execute(context.Background(), order(quote(t, "USDC", "SOL", "100", "0.8")))
	if !errors.Is(err, ErrInsufficientBalance) || !strings.Contains(err.Error(), "network fee") {
		t.Fatalf("err = %v, want insufficient SOL for network fee", err)
	}
}

func TestSellingAllSOLLeavesNothingForFee(t *testing.T) {
	l, _ := newLedger(t, map[string]string{"SOL": "1"})
	ex := NewPaperExecutor(l, DefaultLimits, CostEstimator{Fees: DefaultPaperFees, SOL: tok(t, "SOL")})
	if _, err := ex.Execute(context.Background(), order(quote(t, "SOL", "USDC", "1", "120"))); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if _, err := ex.Execute(context.Background(), order(quote(t, "SOL", "USDC", "0.99", "118"))); err != nil {
		t.Fatal(err)
	}
	if b := balances(t, l); b["SOL"] != "0.009895" {
		t.Errorf("SOL = %s", b["SOL"])
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
