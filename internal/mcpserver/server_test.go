package mcpserver

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentic-trader/internal/memory"
	"agentic-trader/internal/store"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

// fakeVenue quotes 1 USDC = 0.01 SOL.
type fakeVenue struct{ tokens *venue.TokenRegistry }

func (fakeVenue) Name() string { return "fake" }

func (f fakeVenue) Quote(_ context.Context, r venue.QuoteRequest) (*venue.Quote, error) {
	from, err := f.tokens.Lookup(r.From)
	if err != nil {
		return nil, err
	}
	to, err := f.tokens.Lookup(r.To)
	if err != nil {
		return nil, err
	}
	in, err := venue.ParseUnits(r.Amount, from.Decimals)
	if err != nil {
		return nil, err
	}
	out := new(big.Int).Div(new(big.Int).Mul(in, big.NewInt(10_000_000)), big.NewInt(1_000_000)) // USDC(6) -> SOL(9) at 0.01
	return &venue.Quote{Venue: "fake", From: from, To: to, InAmount: in, OutAmount: out, MinOutAmount: out, FetchedAt: time.Now()}, nil
}

func connect(t *testing.T) (*mcp.ClientSession, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	ctx0 := context.Background()
	var toks []store.Token
	for _, tk := range venue.SolanaTokens {
		toks = append(toks, store.Token{Symbol: tk.Symbol, Mint: tk.Address, Decimals: tk.Decimals})
	}
	if err := db.UpsertTokens(ctx0, toks); err != nil {
		t.Fatal(err)
	}
	for _, d := range []store.Deposit{
		{At: time.Now(), Symbol: "USDC", Amount: big.NewInt(1000e6), ValueUSDC: "1000"},
		{At: time.Now(), Symbol: "SOL", Amount: big.NewInt(1e8), ValueUSDC: "1"},
	} {
		if err := db.AddDeposit(ctx0, d); err != nil {
			t.Fatal(err)
		}
	}
	ledger := trading.NewLedger(db, tokens)
	sol, _ := tokens.Lookup("SOL")
	costs := trading.CostEstimator{Fees: trading.DefaultPaperFees, Pricer: trading.VenuePricer{Venue: fakeVenue{tokens}}, SOL: sol}
	s := New(Config{
		Venue:    fakeVenue{tokens},
		Executor: trading.NewPaperExecutor(ledger, trading.DefaultLimits, costs),
		Costs:    costs,
		Ledger:   ledger,
		Memory:   memory.New(db, filepath.Join(dir, "NARRATIVE.md"), 0),
		Store:    db,
		QuoteTTL: trading.DefaultLimits.MaxQuoteAge,
		RunID:    "run-test",
	})

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, db, dir
}

func call[T any](t *testing.T, cs *mcp.ClientSession, name string, args any) (T, *mcp.CallToolResult) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out T
	if !res.IsError {
		data, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
	}
	return out, res
}

func errText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func TestQuoteExecuteFlow(t *testing.T) {
	cs, _, _ := connect(t)

	q, res := call[QuoteOutput](t, cs, "get_quote", map[string]any{"from": "USDC", "to": "SOL", "amount": "100"})
	if res.IsError {
		t.Fatalf("get_quote: %s", errText(res))
	}
	if q.Out != "1" || !strings.HasPrefix(q.QuoteID, "q-") || q.ExpiresAt.IsZero() || q.Costs.NetworkFeeSOL != "0.000105" {
		t.Errorf("quote = %+v", q)
	}

	fill, res := call[ExecuteOutput](t, cs, "execute", map[string]any{"quote_id": q.QuoteID, "reason": "test"})
	if res.IsError {
		t.Fatalf("execute: %s", errText(res))
	}
	// 0.1 + 1 bought - 0.000105 network fee
	if fill.TradeID == "" || fill.Balances["USDC"] != "900" || fill.Balances["SOL"] != "1.099895" || fill.FeeSOL != "0.000105" {
		t.Errorf("fill = %+v", fill)
	}

	// A quote executes at most once.
	_, res = call[ExecuteOutput](t, cs, "execute", map[string]any{"quote_id": q.QuoteID, "reason": "test"})
	if !res.IsError || !strings.Contains(errText(res), "unknown or expired") {
		t.Errorf("re-execute: isError=%v %q", res.IsError, errText(res))
	}

	bal, _ := call[BalancesOutput](t, cs, "get_balances", map[string]any{})
	if bal.Balances["USDC"] != "900" {
		t.Errorf("balances = %v", bal.Balances)
	}
}

func TestErrorsReachModel(t *testing.T) {
	cs, _, _ := connect(t)

	_, res := call[QuoteOutput](t, cs, "get_quote", map[string]any{"from": "NOPE", "to": "SOL", "amount": "1"})
	if !res.IsError || !strings.Contains(errText(res), "unknown token") {
		t.Errorf("unknown token: isError=%v %q", res.IsError, errText(res))
	}

	q, _ := call[QuoteOutput](t, cs, "get_quote", map[string]any{"from": "USDC", "to": "SOL", "amount": "5000"})
	_, res = call[ExecuteOutput](t, cs, "execute", map[string]any{"quote_id": q.QuoteID, "reason": "test"})
	if !res.IsError || !strings.Contains(errText(res), "insufficient balance") {
		t.Errorf("overdraw: isError=%v %q", res.IsError, errText(res))
	}
}

func TestNarrativeLinksRunTrades(t *testing.T) {
	cs, db, dir := connect(t)

	q, _ := call[QuoteOutput](t, cs, "get_quote", map[string]any{"from": "USDC", "to": "SOL", "amount": "10"})
	// reason is required by the input schema.
	_, res := call[ExecuteOutput](t, cs, "execute", map[string]any{"quote_id": q.QuoteID})
	if !res.IsError {
		t.Fatal("execute without reason should fail")
	}
	q, _ = call[QuoteOutput](t, cs, "get_quote", map[string]any{"from": "USDC", "to": "SOL", "amount": "10"})
	fill, res := call[ExecuteOutput](t, cs, "execute", map[string]any{"quote_id": q.QuoteID, "reason": "range bottom"})
	if res.IsError {
		t.Fatalf("execute: %s", errText(res))
	}

	_, res = call[UpdateNarrativeOutput](t, cs, "update_narrative", map[string]any{"narrative": "## Market view\nonly this", "summary": "x"})
	if !res.IsError || !strings.Contains(errText(res), "missing required section") {
		t.Errorf("invalid narrative: isError=%v %q", res.IsError, errText(res))
	}

	out, res := call[UpdateNarrativeOutput](t, cs, "update_narrative", map[string]any{"narrative": memory.Template(), "summary": "bought SOL"})
	if res.IsError {
		t.Fatalf("update_narrative: %s", errText(res))
	}
	if out.RunID != "run-test" || len(out.TradeIDs) != 1 || out.TradeIDs[0] != fill.TradeID {
		t.Errorf("out = %+v, want trade %s", out, fill.TradeID)
	}
	trades, _ := db.Trades(context.Background())
	if len(trades) != 1 || trades[0].Reason != "range bottom" || trades[0].RunID != "run-test" {
		t.Errorf("trades = %+v", trades)
	}
	// Both quotes were recorded; only the executed one is linked to the trade.
	if id, found, err := db.QuoteTrade(context.Background(), q.QuoteID); err != nil || !found || id != fill.TradeID {
		t.Errorf("quote link = %q found=%v err=%v", id, found, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "NARRATIVE.md")); err != nil {
		t.Error(err)
	}
}
