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

func connect(t *testing.T) (*mcp.ClientSession, string) {
	t.Helper()
	dir := t.TempDir()
	wp := filepath.Join(dir, "WALLET.json")
	if err := os.WriteFile(wp, []byte(`{"balances":{"USDC":"1000","SOL":"0.1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	ledger, err := trading.OpenLedger(wp, filepath.Join(dir, "trades.jsonl"), tokens)
	if err != nil {
		t.Fatal(err)
	}
	sol, _ := tokens.Lookup("SOL")
	costs := trading.CostEstimator{Fees: trading.DefaultPaperFees, Pricer: trading.VenuePricer{Venue: fakeVenue{tokens}}, SOL: sol}
	s := New(Config{
		Venue:    fakeVenue{tokens},
		Executor: trading.NewPaperExecutor(ledger, trading.DefaultLimits, costs),
		Costs:    costs,
		Ledger:   ledger,
		Memory:   memory.NewStore(filepath.Join(dir, "NARRATIVE.md"), filepath.Join(dir, "journal.jsonl"), 0),
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
	return cs, dir
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
	cs, _ := connect(t)

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
	cs, _ := connect(t)

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
	cs, dir := connect(t)

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
	trades, _ := os.ReadFile(filepath.Join(dir, "trades.jsonl"))
	if !strings.Contains(string(trades), `"reason":"range bottom"`) || !strings.Contains(string(trades), `"run_id":"run-test"`) {
		t.Errorf("trades = %s", trades)
	}
	if _, err := os.Stat(filepath.Join(dir, "NARRATIVE.md")); err != nil {
		t.Error(err)
	}
}
