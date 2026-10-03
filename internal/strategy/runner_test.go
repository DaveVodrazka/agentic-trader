package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

// fixedMarket quotes and prices tokens at fixed USD prices.
type fixedMarket map[string]float64

func (fixedMarket) Name() string { return "fixed" }

func (m fixedMarket) Quote(_ context.Context, r venue.QuoteRequest) (*venue.Quote, error) {
	from, _ := tokens.Lookup(r.From)
	to, _ := tokens.Lookup(r.To)
	in, err := venue.ParseUnits(r.Amount, from.Decimals)
	if err != nil {
		return nil, err
	}
	amt, _ := new(big.Rat).SetString(r.Amount)
	out := new(big.Rat).Mul(amt, new(big.Rat).SetFloat64(m[r.From]/m[r.To]))
	out.Mul(out, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(to.Decimals)), nil)))
	n := new(big.Int).Quo(out.Num(), out.Denom())
	return &venue.Quote{Venue: "fixed", From: from, To: to, InAmount: in, OutAmount: n, MinOutAmount: n, FetchedAt: time.Now()}, nil
}

func (m fixedMarket) USDPrices(_ context.Context, toks []venue.Token) (map[string]float64, error) {
	out := map[string]float64{}
	for _, t := range toks {
		if p, ok := m[t.Symbol]; ok {
			out[t.Symbol] = p
		}
	}
	return out, nil
}

func newRunner(t *testing.T, market fixedMarket, now time.Time) (*Runner, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	var toks []store.Token
	var universe []venue.Token
	for _, tk := range venue.SolanaTokens {
		toks = append(toks, store.Token{Symbol: tk.Symbol, Mint: tk.Address, Decimals: tk.Decimals})
		if tk.Symbol != Cash {
			universe = append(universe, tk)
		}
	}
	must(t, st.UpsertTokens(ctx, toks))
	must(t, st.AddDeposit(ctx, store.Deposit{At: now, Symbol: "USDC", Amount: big.NewInt(1000e6), ValueUSDC: "1000"}))
	must(t, st.AddDeposit(ctx, store.Deposit{At: now, Symbol: "SOL", Amount: big.NewInt(5e7), ValueUSDC: "5"}))
	sol, _ := tokens.Lookup("SOL")
	costs := trading.CostEstimator{Fees: trading.DefaultPaperFees, SOL: sol}
	return &Runner{
		Store: st, Tokens: tokens, Universe: universe, Prices: market, Venue: market,
		Executor: trading.NewPaperExecutor(trading.NewLedger(st, tokens), trading.DefaultLimits, costs),
		Rules:    DefaultRules, Now: func() time.Time { return now },
	}, st
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunnerTick(t *testing.T) {
	now := time.Now().UTC()
	market := fixedMarket{"SOL": 100, "USDC": 1, "WIF": 0.25}
	r, st := newRunner(t, market, now)
	ctx := context.Background()

	// No strategy: prices are still recorded.
	res, err := r.Tick(ctx)
	if err != nil || res.Skipped != "no strategy set" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if cs, _ := st.Candles(ctx, "SOL", "1h", now.Add(-time.Hour), now.Add(time.Hour)); len(cs) != 1 || cs[0].Close != 100 {
		t.Errorf("candles = %+v", cs)
	}

	act, err := Activate(ctx, st, tokens, "rebalance", json.RawMessage(`{"weights":{"SOL":0.5}}`), "test", "user", "", now, false)
	must(t, err)
	res, err = r.Tick(ctx)
	must(t, err)
	if len(res.Actions) != 1 || res.Actions[0].TradeID == "" || res.Actions[0].Error != "" {
		t.Fatalf("actions = %+v", res.Actions)
	}

	b, _ := st.Balances(ctx)
	solUnits := float64(b["SOL"].Int64()) / 1e9
	// Portfolio $1005: reserve $5, investable $1000 -> SOL target $505 -> 5.05 SOL (minus fee).
	if solUnits < 5.04 || solUnits > 5.051 {
		t.Errorf("SOL = %v", solUnits)
	}
	trades, _ := st.Trades(ctx)
	if len(trades) != 1 || trades[0].ActivationID != act.ID {
		t.Errorf("trades = %+v", trades)
	}
	live, _, _ := st.LiveStrategy(ctx)
	var state State
	must(t, json.Unmarshal([]byte(live.State), &state))
	if state.Entry["SOL"] < 99 || state.Entry["SOL"] > 101 || state.Peak < 1004 {
		t.Errorf("state = %+v", state)
	}
	ticks, _ := st.RecentTicks(ctx, act.ID, 5)
	if len(ticks) != 1 {
		t.Errorf("ticks = %+v", ticks)
	}

	// At target: next tick trades nothing.
	res, _ = r.Tick(ctx)
	if len(res.Actions) != 0 {
		t.Errorf("second tick actions = %+v", res.Actions)
	}

	// A 30% SOL crash trips the stop-loss and sells.
	market["SOL"] = 70
	res, _ = r.Tick(ctx)
	if len(res.Actions) == 0 || res.Actions[0].From != "SOL" || res.Actions[0].TradeID == "" {
		t.Errorf("stop-loss actions = %+v note=%s", res.Actions, res.Plan.Note())
	}
}

func TestActivateRules(t *testing.T) {
	now := time.Now().UTC()
	r, st := newRunner(t, fixedMarket{"SOL": 100}, now)
	_ = r
	ctx := context.Background()
	if _, err := Activate(ctx, st, tokens, "trend", nil, "", "user", "", now, false); err == nil {
		t.Error("empty reason accepted")
	}
	if _, err := Activate(ctx, st, tokens, "trend", json.RawMessage(`{"fast":99}`), "x", "user", "", now, false); err == nil {
		t.Error("invalid params accepted")
	}
	a, err := Activate(ctx, st, tokens, "trend", nil, "x", "user", "", now, false)
	must(t, err)
	if _, err := Activate(ctx, st, tokens, "cash", nil, "y", "agent", "", now.Add(time.Hour), false); !errors.Is(err, ErrTooSoon) {
		t.Errorf("err = %v, want ErrTooSoon", err)
	}
	if _, err := Activate(ctx, st, tokens, "cash", nil, "y", "user", "", now.Add(time.Hour), true); err != nil {
		t.Errorf("force: %v", err)
	}
	must(t, st.SaveStrategyState(ctx, a.ID+1, "{}", "drawdown"))
	if _, err := Activate(ctx, st, tokens, "hold", nil, "z", "agent", "", now.Add(2*time.Hour), false); err != nil {
		t.Errorf("halted strategy should be replaceable: %v", err)
	}
}
