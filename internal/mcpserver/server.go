// Package mcpserver exposes the portfolio-manager tools over MCP so an LLM
// agent (claude -p) can study the market, test strategies, choose which one
// trades, and keep its narrative. The agent does not place trades itself:
// the live strategy does, on every tick.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentic-trader/internal/memory"
	"agentic-trader/internal/store"
	"agentic-trader/internal/strategy"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

// Config holds the dependencies behind the tools.
type Config struct {
	Store   *store.Store
	Tokens  *venue.TokenRegistry
	Ledger  *trading.Ledger
	Memory  *memory.Memory
	Symbols []string // tokens strategies may trade
	RunID   string   // this agent activation
	Now     func() time.Time
}

// Server implements the MCP tools.
type Server struct{ cfg Config }

// New returns a Server for cfg.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg}
}

// Limits on agent requests, to keep tool output small.
const (
	maxCandles      = 100
	maxBacktestDays = 40 // hourly history covers ~41 days
)

// MCP returns an MCP server with the tools registered.
func (s *Server) MCP() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "trader", Version: "0.2.0"}, nil)
	add := func(name, desc string) *mcp.Tool { return &mcp.Tool{Name: name, Description: desc} }

	mcp.AddTool(srv, add("market_summary",
		"Per-token market overview from stored price bars: price, 24h/7d/30d change, 30d volatility, hourly and daily "+
			"trend (MA20 vs MA50), RSI, position in the 30-day range, 30-day max drawdown, correlation to SOL, and data "+
			"freshness. Start every run here."), s.marketSummary)
	mcp.AddTool(srv, add("get_candles",
		fmt.Sprintf("Raw OHLC price bars for one token, newest last. interval: 1h or 1d. At most %d bars.", maxCandles)),
		s.getCandles)
	mcp.AddTool(srv, add("list_strategies",
		"The available trading strategies with what they do and their default params."), s.listStrategies)
	mcp.AddTool(srv, add("compare_strategies",
		fmt.Sprintf("Backtest every strategy at default params plus benchmarks (hold SOL, 50/50) over the last `days` "+
			"(1-%d), ranked by return. Includes trading costs.", maxBacktestDays)), s.compareStrategies)
	mcp.AddTool(srv, add("backtest",
		fmt.Sprintf("Backtest one strategy with specific params over the last `days` (1-%d), next to the benchmarks. "+
			"Use to test a param choice before set_strategy. Short windows overfit: prefer evidence that holds over "+
			"several windows.", maxBacktestDays)), s.backtest)
	mcp.AddTool(srv, add("strategy_status",
		"The live strategy: params, reason, how long it has run, its return since activation vs the benchmarks over "+
			"the same window, trades and costs, entry prices, stop-outs, last decision, and when it may be replaced."),
		s.strategyStatus)
	mcp.AddTool(srv, add("set_strategy",
		fmt.Sprintf("Make a strategy live; it trades from the next tick. `reason` is required: the evidence and what "+
			"would make you switch. A healthy strategy must run %s before it can be replaced; a halted one can be "+
			"replaced any time. Switching costs fees, so only switch on clear evidence.", strategy.MinHold)), s.setStrategy)
	mcp.AddTool(srv, add("get_balances", "Current wallet balances by token symbol."), s.getBalances)
	mcp.AddTool(srv, add("update_narrative",
		fmt.Sprintf("Save your narrative (working memory for your next run) and a journal entry for this run. Call once "+
			"at the end of every run, even if nothing changed. `narrative` replaces the previous one: markdown, max %d "+
			"bytes, with exactly these headings:\n%s`summary` is what you observed, decided and why.",
			s.cfg.Memory.MaxBytes(), memory.Template())), s.updateNarrative)
	return srv
}

// history loads the price bars the tools need.
func (s *Server) history(ctx context.Context, days int) (*strategy.MemData, error) {
	now := s.cfg.Now().UTC()
	return strategy.LoadHistory(ctx, s.cfg.Store, s.cfg.Symbols, now.Add(-time.Duration(days+150)*24*time.Hour), now)
}

// ---- market_summary ----------------------------------------------------------

// MarketSummaryOutput is the output of market_summary.
type MarketSummaryOutput struct {
	AsOf   time.Time             `json:"as_of"`
	Tokens []strategy.TokenStats `json:"tokens"`
}

func (s *Server) marketSummary(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, MarketSummaryOutput, error) {
	data, err := s.history(ctx, 60)
	if err != nil {
		return nil, MarketSummaryOutput{}, err
	}
	return nil, MarketSummaryOutput{AsOf: data.Now, Tokens: strategy.Summarize(data, s.cfg.Symbols)}, nil
}

// ---- get_candles ---------------------------------------------------------------

// GetCandlesInput is the input of get_candles.
type GetCandlesInput struct {
	Symbol   string `json:"symbol" jsonschema:"token symbol, e.g. SOL"`
	Interval string `json:"interval" jsonschema:"1h or 1d"`
	Limit    int    `json:"limit,omitempty" jsonschema:"number of bars, default 48, max 100"`
}

// Bar is one OHLC bar.
type Bar struct {
	Start time.Time `json:"start"`
	Open  float64   `json:"open"`
	High  float64   `json:"high"`
	Low   float64   `json:"low"`
	Close float64   `json:"close"`
}

// GetCandlesOutput is the output of get_candles.
type GetCandlesOutput struct {
	Symbol   string `json:"symbol"`
	Interval string `json:"interval"`
	Bars     []Bar  `json:"bars"`
}

func (s *Server) getCandles(ctx context.Context, _ *mcp.CallToolRequest, in GetCandlesInput) (*mcp.CallToolResult, GetCandlesOutput, error) {
	dur, ok := strategy.Intervals[in.Interval]
	if !ok || in.Interval == "5m" {
		return nil, GetCandlesOutput{}, errors.New("interval must be 1h or 1d")
	}
	tok, err := s.cfg.Tokens.Lookup(in.Symbol)
	if err != nil {
		return nil, GetCandlesOutput{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 48
	}
	limit = min(limit, maxCandles)
	now := s.cfg.Now().UTC()
	cs, err := s.cfg.Store.Candles(ctx, tok.Symbol, in.Interval, now.Add(-time.Duration(limit+1)*dur), now.Add(time.Second))
	if err != nil {
		return nil, GetCandlesOutput{}, err
	}
	cs = cs[max(0, len(cs)-limit):]
	out := GetCandlesOutput{Symbol: tok.Symbol, Interval: in.Interval, Bars: make([]Bar, len(cs))}
	for i, c := range cs {
		out.Bars[i] = Bar{Start: c.Start, Open: c.Open, High: c.High, Low: c.Low, Close: c.Close}
	}
	return nil, out, nil
}

// ---- strategies ------------------------------------------------------------------

// StrategyInfo describes a strategy.
type StrategyInfo struct {
	Name     string         `json:"name"`
	Summary  string         `json:"summary"`
	Defaults map[string]any `json:"default_params"`
}

// ListStrategiesOutput is the output of list_strategies.
type ListStrategiesOutput struct {
	Strategies []StrategyInfo `json:"strategies"`
	Rules      string         `json:"risk_rules" jsonschema:"safety rules applied to every strategy"`
}

func (s *Server) listStrategies(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, ListStrategiesOutput, error) {
	var out ListStrategiesOutput
	for _, sp := range strategy.Specs() {
		defaults := map[string]any{}
		_ = json.Unmarshal(sp.DefaultParams(), &defaults)
		out.Strategies = append(out.Strategies, StrategyInfo{Name: sp.Name, Summary: sp.Summary, Defaults: defaults})
	}
	r := strategy.DefaultRules
	out.Rules = fmt.Sprintf("max %.0f%% per token; trades under $%.0f or %.0f%% of the portfolio skipped; %.2g SOL kept for "+
		"fees; per-token stop-loss at -%.0f%% from entry (stopped tokens not re-bought until a new strategy is set); "+
		"portfolio drawdown of %.0f%% from peak halts the strategy (all cash) until a new one is set. Tradable tokens: %s.",
		r.MaxWeight*100, r.MinTradeUSD, r.MinTradeFrac*100, r.SOLReserve, r.StopLoss*100, r.MaxDrawdown*100,
		strings.Join(s.cfg.Symbols, ", "))
	return nil, out, nil
}

// CompareInput is the input of compare_strategies.
type CompareInput struct {
	Days int `json:"days" jsonschema:"backtest window in days, 1-40"`
}

// CompareOutput is the output of compare_strategies.
type CompareOutput struct {
	Days    int                `json:"days"`
	Ranking []strategy.Summary `json:"ranking"`
}

func (s *Server) compareStrategies(ctx context.Context, _ *mcp.CallToolRequest, in CompareInput) (*mcp.CallToolResult, CompareOutput, error) {
	cfg, data, err := s.backtestSetup(ctx, in.Days)
	if err != nil {
		return nil, CompareOutput{}, err
	}
	rows, err := strategy.Compare(data, s.cfg.Symbols, cfg, s.cfg.Tokens)
	return nil, CompareOutput{Days: in.Days, Ranking: rows}, err
}

// BacktestInput is the input of backtest.
type BacktestInput struct {
	Strategy string         `json:"strategy"`
	Params   map[string]any `json:"params,omitempty" jsonschema:"strategy params; omitted fields use defaults"`
	Days     int            `json:"days" jsonschema:"backtest window in days, 1-40"`
}

// BacktestOutput is the output of backtest.
type BacktestOutput struct {
	Days       int                `json:"days"`
	Result     strategy.Summary   `json:"result"`
	Benchmarks []strategy.Summary `json:"benchmarks"`
}

func (s *Server) backtest(ctx context.Context, _ *mcp.CallToolRequest, in BacktestInput) (*mcp.CallToolResult, BacktestOutput, error) {
	spec, err := strategy.Lookup(in.Strategy)
	if err != nil {
		return nil, BacktestOutput{}, err
	}
	params, err := rawParams(in.Params)
	if err != nil {
		return nil, BacktestOutput{}, err
	}
	cfg, data, err := s.backtestSetup(ctx, in.Days)
	if err != nil {
		return nil, BacktestOutput{}, err
	}
	res, bench, err := strategy.RunBacktest(spec, params, data, s.cfg.Symbols, cfg, s.cfg.Tokens)
	return nil, BacktestOutput{Days: in.Days, Result: res, Benchmarks: bench}, err
}

func (s *Server) backtestSetup(ctx context.Context, days int) (strategy.BacktestConfig, *strategy.MemData, error) {
	if days < 1 || days > maxBacktestDays {
		return strategy.BacktestConfig{}, nil, fmt.Errorf("days must be between 1 and %d", maxBacktestDays)
	}
	data, err := s.history(ctx, days)
	if err != nil {
		return strategy.BacktestConfig{}, nil, err
	}
	return strategy.DefaultBacktest(data.Now.Truncate(time.Hour), days), data, nil
}

// StrategyStatusOutput is the output of strategy_status.
type StrategyStatusOutput struct {
	Live        bool                  `json:"live"`
	Performance *strategy.Performance `json:"performance,omitempty"`
	Note        string                `json:"note,omitempty"`
}

func (s *Server) strategyStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, StrategyStatusOutput, error) {
	p, ok, err := strategy.LivePerformance(ctx, s.cfg.Store, s.cfg.Symbols, s.cfg.Tokens, s.cfg.Now().UTC())
	if err != nil {
		return nil, StrategyStatusOutput{}, err
	}
	if !ok {
		return nil, StrategyStatusOutput{Note: "no strategy is live; nothing trades until you call set_strategy"}, nil
	}
	return nil, StrategyStatusOutput{Live: true, Performance: &p}, nil
}

// SetStrategyInput is the input of set_strategy.
type SetStrategyInput struct {
	Strategy string         `json:"strategy"`
	Params   map[string]any `json:"params,omitempty" jsonschema:"strategy params; omitted fields use defaults"`
	Reason   string         `json:"reason" jsonschema:"evidence for this choice and what would make you switch"`
}

// SetStrategyOutput is the output of set_strategy.
type SetStrategyOutput struct {
	ActivationID int64  `json:"activation_id"`
	Strategy     string `json:"strategy"`
	Params       string `json:"params" jsonschema:"effective params including defaults"`
	Note         string `json:"note"`
}

func (s *Server) setStrategy(ctx context.Context, _ *mcp.CallToolRequest, in SetStrategyInput) (*mcp.CallToolResult, SetStrategyOutput, error) {
	params, err := rawParams(in.Params)
	if err != nil {
		return nil, SetStrategyOutput{}, err
	}
	a, err := strategy.Activate(ctx, s.cfg.Store, s.cfg.Tokens, in.Strategy, params, in.Reason, "agent", s.cfg.RunID,
		s.cfg.Now().UTC(), false)
	if err != nil {
		return nil, SetStrategyOutput{}, err
	}
	return nil, SetStrategyOutput{ActivationID: a.ID, Strategy: a.Strategy, Params: a.Params,
		Note: "live; it trades on the next tick (within a few minutes)"}, nil
}

func rawParams(m map[string]any) (json.RawMessage, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return json.Marshal(m)
}

// ---- wallet & memory ---------------------------------------------------------------

// BalancesOutput is the output of get_balances.
type BalancesOutput struct {
	Balances map[string]string `json:"balances"`
}

func (s *Server) getBalances(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, BalancesOutput, error) {
	b, err := s.cfg.Ledger.Balances(ctx)
	if err != nil {
		return nil, BalancesOutput{}, err
	}
	return nil, BalancesOutput{Balances: b}, nil
}

// UpdateNarrativeInput is the input of update_narrative.
type UpdateNarrativeInput struct {
	Narrative string `json:"narrative" jsonschema:"full replacement narrative in markdown with the required headings"`
	Summary   string `json:"summary" jsonschema:"journal entry for this run: what you observed, decided and why"`
}

// UpdateNarrativeOutput is the output of update_narrative.
type UpdateNarrativeOutput struct {
	RunID string `json:"run_id"`
	Saved bool   `json:"saved"`
}

func (s *Server) updateNarrative(ctx context.Context, _ *mcp.CallToolRequest, in UpdateNarrativeInput) (*mcp.CallToolResult, UpdateNarrativeOutput, error) {
	if err := s.cfg.Memory.Update(ctx, s.cfg.RunID, in.Narrative, in.Summary); err != nil {
		return nil, UpdateNarrativeOutput{}, err
	}
	return nil, UpdateNarrativeOutput{RunID: s.cfg.RunID, Saved: true}, nil
}
