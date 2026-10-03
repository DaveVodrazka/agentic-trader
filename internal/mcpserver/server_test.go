package mcpserver

import (
	"context"
	"encoding/json"
	"math/big"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentic-trader/internal/memory"
	"agentic-trader/internal/store"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

var now = time.Date(2026, 10, 3, 14, 30, 0, 0, time.UTC)

func connect(t *testing.T) (*mcp.ClientSession, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	var toks []store.Token
	for _, tk := range venue.SolanaTokens {
		toks = append(toks, store.Token{Symbol: tk.Symbol, Mint: tk.Address, Decimals: tk.Decimals})
	}
	must(t, db.UpsertTokens(ctx, toks))
	must(t, db.AddDeposit(ctx, store.Deposit{At: now, Symbol: "USDC", Amount: big.NewInt(1000e6), ValueUSDC: "1000"}))

	// 900 hourly and 90 daily SOL bars, gently rising, ending at "now".
	var cs []store.Candle
	for i := 0; i < 900; i++ {
		p := 100 + float64(i)*0.02
		cs = append(cs, store.Candle{Symbol: "SOL", Interval: "1h", Start: now.Truncate(time.Hour).Add(-time.Duration(899-i) * time.Hour),
			Open: p, High: p, Low: p, Close: p, Source: "test"})
	}
	for i := 0; i < 90; i++ {
		p := 80 + float64(i)*0.4
		cs = append(cs, store.Candle{Symbol: "SOL", Interval: "1d", Start: now.Truncate(24 * time.Hour).Add(-time.Duration(90-i) * 24 * time.Hour),
			Open: p, High: p, Low: p, Close: p, Source: "test"})
	}
	must(t, db.UpsertCandles(ctx, cs, store.ReplaceAll))

	s := New(Config{
		Store: db, Tokens: tokens, Ledger: trading.NewLedger(db, tokens),
		Memory:  memory.New(db, filepath.Join(t.TempDir(), "NARRATIVE.md"), 0),
		Symbols: []string{"SOL", "JUP"}, RunID: "run-test", Now: func() time.Time { return now },
	})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs2, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs2.Close() })
	return cs2, db
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
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

func TestToolSet(t *testing.T) {
	cs, _ := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	must(t, err)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := "backtest,compare_strategies,get_balances,get_candles,list_strategies,market_summary,set_strategy,strategy_status,update_narrative"
	if strings.Join(names, ",") != want {
		t.Errorf("tools = %v", names)
	}
}

func TestMarketTools(t *testing.T) {
	cs, _ := connect(t)
	sum, res := call[MarketSummaryOutput](t, cs, "market_summary", map[string]any{})
	if res.IsError {
		t.Fatal(errText(res))
	}
	if len(sum.Tokens) != 2 || sum.Tokens[0].Symbol != "SOL" || sum.Tokens[0].Trend1h != "up" || sum.Tokens[1].Price != "" {
		t.Errorf("summary = %+v", sum)
	}

	bars, res := call[GetCandlesOutput](t, cs, "get_candles", map[string]any{"symbol": "SOL", "interval": "1h", "limit": 500})
	if res.IsError || len(bars.Bars) != maxCandles || !bars.Bars[0].Start.Before(bars.Bars[99].Start) {
		t.Errorf("candles: %d bars, err=%s", len(bars.Bars), errText(res))
	}
	_, res = call[GetCandlesOutput](t, cs, "get_candles", map[string]any{"symbol": "SOL", "interval": "5m"})
	if !res.IsError {
		t.Error("5m should be rejected")
	}
}

func TestStrategyTools(t *testing.T) {
	cs, db := connect(t)

	list, _ := call[ListStrategiesOutput](t, cs, "list_strategies", map[string]any{})
	if len(list.Strategies) != 6 || !strings.Contains(list.Rules, "stop-loss") {
		t.Errorf("list = %+v", list)
	}

	cmp, res := call[CompareOutput](t, cs, "compare_strategies", map[string]any{"days": 14})
	if res.IsError || len(cmp.Ranking) != 8 {
		t.Fatalf("compare: %+v %s", cmp, errText(res))
	}
	_, res = call[CompareOutput](t, cs, "compare_strategies", map[string]any{"days": 90})
	if !res.IsError {
		t.Error("days > 40 should be rejected")
	}

	bt, res := call[BacktestOutput](t, cs, "backtest", map[string]any{"strategy": "trend", "params": map[string]any{"fast": 10, "slow": 30}, "days": 14})
	if res.IsError || !strings.Contains(bt.Result.Params, `"fast":10`) || len(bt.Benchmarks) != 2 {
		t.Errorf("backtest: %+v %s", bt, errText(res))
	}
	_, res = call[BacktestOutput](t, cs, "backtest", map[string]any{"strategy": "trend", "params": map[string]any{"bogus": 1}, "days": 14})
	if !res.IsError {
		t.Error("unknown param should be rejected")
	}

	status, _ := call[StrategyStatusOutput](t, cs, "strategy_status", map[string]any{})
	if status.Live {
		t.Error("no strategy should be live")
	}

	set, res := call[SetStrategyOutput](t, cs, "set_strategy", map[string]any{"strategy": "voltarget", "reason": "calm market"})
	if res.IsError || set.ActivationID == 0 {
		t.Fatalf("set: %+v %s", set, errText(res))
	}
	_, res = call[SetStrategyOutput](t, cs, "set_strategy", map[string]any{"strategy": "cash", "reason": "changed my mind"})
	if !res.IsError || !strings.Contains(errText(res), "can switch in") {
		t.Errorf("min hold not enforced: %s", errText(res))
	}

	status, _ = call[StrategyStatusOutput](t, cs, "strategy_status", map[string]any{})
	if !status.Live || status.Performance.Strategy != "voltarget" || status.Performance.CanSwitchIn == "" {
		t.Errorf("status = %+v", status)
	}
	a, _, _ := db.LiveStrategy(context.Background())
	if a.SetBy != "agent" || a.RunID != "run-test" {
		t.Errorf("activation = %+v", a)
	}
}

func TestNarrativeAndBalances(t *testing.T) {
	cs, db := connect(t)
	bal, _ := call[BalancesOutput](t, cs, "get_balances", map[string]any{})
	if bal.Balances["USDC"] != "1000" {
		t.Errorf("balances = %+v", bal)
	}
	_, res := call[UpdateNarrativeOutput](t, cs, "update_narrative", map[string]any{"narrative": "## Market view\nx", "summary": "s"})
	if !res.IsError || !strings.Contains(errText(res), "## Active strategy") {
		t.Errorf("missing sections not reported: %s", errText(res))
	}
	out, res := call[UpdateNarrativeOutput](t, cs, "update_narrative", map[string]any{"narrative": memory.Template(), "summary": "chose voltarget"})
	if res.IsError || !out.Saved {
		t.Fatalf("update: %s", errText(res))
	}
	if j, _ := db.Journal(context.Background(), 0); len(j) != 1 || j[0].RunID != "run-test" {
		t.Errorf("journal = %+v", j)
	}
}
