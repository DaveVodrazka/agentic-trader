// Package mcpserver exposes trading tools (quote, execute, balances) over MCP
// so an LLM agent such as `claude -p` can trade.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentic-trader/internal/memory"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

// ErrUnknownQuote means a quote_id was never issued, already executed, or expired.
var ErrUnknownQuote = errors.New("unknown or expired quote_id; call get_quote again")

// Config holds the dependencies behind the MCP tools.
type Config struct {
	Venue    venue.Venue
	Executor trading.Executor
	Ledger   *trading.Ledger
	Memory   *memory.Store
	Costs    trading.CostEstimator
	// QuoteTTL should match the executor's MaxQuoteAge so the expires_at
	// reported to the model is accurate.
	QuoteTTL time.Duration
	// RunID identifies this agent activation; it tags trades and the journal.
	RunID string
}

// Server implements the MCP tools. Quotes are kept server-side and
// referenced by ID so the model cannot alter amounts.
type Server struct {
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	quotes   map[string]*venue.Quote
	tradeIDs []string // trades executed during this run
}

// New returns a Server for cfg.
func New(cfg Config) *Server {
	return &Server{cfg: cfg, now: time.Now, quotes: make(map[string]*venue.Quote)}
}

// MCP returns an MCP server with the trading tools registered.
func (s *Server) MCP() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "trader", Version: "0.1.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_quote",
		Description: "Get a swap quote on Solana via " + s.cfg.Venue.Name() + ". " +
			"Sells `amount` of `from` for `to`. Returns a quote_id to pass to execute. " +
			"Quotes expire quickly (see expires_at); re-quote if execute reports it stale.",
	}, s.getQuote)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "execute",
		Description: "Execute a previously fetched quote by quote_id. Each quote can be executed once. " +
			"`reason` is required: state the thesis behind the trade and what would invalidate it. " +
			"Returns the fill and updated wallet balances.",
	}, s.execute)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_balances",
		Description: "Get current wallet balances by token symbol.",
	}, s.getBalances)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "update_narrative",
		Description: fmt.Sprintf("Save your trading narrative (working memory for your next run) and a journal entry for this run. "+
			"Call once at the end of every run, including runs with no trades. "+
			"`narrative` replaces the previous one entirely: markdown, max %d bytes, with exactly these headings:\n%s"+
			"Under Positions & plan, cite trade IDs and give an exit/invalidation condition for each position. "+
			"Never state balances as fact in the narrative; get_balances is the source of truth. "+
			"`summary` is what you observed, decided and why during this run.",
			s.cfg.Memory.MaxBytes(), memory.Template()),
	}, s.updateNarrative)
	return srv
}

// GetQuoteInput is the input of get_quote.
type GetQuoteInput struct {
	From        string `json:"from" jsonschema:"token symbol to sell, e.g. USDC"`
	To          string `json:"to" jsonschema:"token symbol to buy, e.g. SOL"`
	Amount      string `json:"amount" jsonschema:"amount of 'from' to sell as a decimal string, e.g. \"100\" or \"0.5\""`
	SlippageBps uint16 `json:"slippage_bps,omitempty" jsonschema:"max slippage in basis points; omit for default (50 = 0.5%)"`
}

// QuoteOutput is the output of get_quote.
type QuoteOutput struct {
	QuoteID        string      `json:"quote_id"`
	Venue          string      `json:"venue"`
	From           string      `json:"from"`
	To             string      `json:"to"`
	In             string      `json:"in"`
	Out            string      `json:"out"`
	MinOut         string      `json:"min_out" jsonschema:"worst-case output after slippage"`
	Price          string      `json:"price" jsonschema:"units of 'to' per one 'from'"`
	PriceImpactPct string      `json:"price_impact_pct" jsonschema:"price impact in percent (1 = 1%)"`
	Costs          CostsOutput `json:"costs" jsonschema:"estimated total cost of executing this quote"`
	SlippageBps    uint16      `json:"slippage_bps"`
	Route          []string    `json:"route"`
	ExpiresAt      time.Time   `json:"expires_at"`
}

func (s *Server) getQuote(ctx context.Context, _ *mcp.CallToolRequest, in GetQuoteInput) (*mcp.CallToolResult, QuoteOutput, error) {
	q, err := s.cfg.Venue.Quote(ctx, venue.QuoteRequest{
		From: in.From, To: in.To, Amount: in.Amount, SlippageBps: in.SlippageBps,
	})
	if err != nil {
		return nil, QuoteOutput{}, err
	}
	id, err := newQuoteID()
	if err != nil {
		return nil, QuoteOutput{}, err
	}

	s.mu.Lock()
	s.pruneLocked()
	s.quotes[id] = q
	s.mu.Unlock()

	return nil, QuoteOutput{
		QuoteID:        id,
		Venue:          q.Venue,
		From:           q.From.Symbol,
		To:             q.To.Symbol,
		In:             q.In(),
		Out:            q.Out(),
		MinOut:         q.MinOut(),
		Price:          q.Price().FloatString(int(q.To.Decimals)),
		PriceImpactPct: q.PriceImpactPercent(),
		Costs:          costsOutput(s.cfg.Costs.Estimate(ctx, q)),
		SlippageBps:    q.SlippageBps,
		Route:          q.Route,
		ExpiresAt:      q.FetchedAt.Add(s.cfg.QuoteTTL).UTC(),
	}, nil
}

// CostsOutput is the cost breakdown shown to the agent. Price impact and
// platform fee are already reflected in `out`; the network fee is charged
// separately in SOL.
type CostsOutput struct {
	NetworkFeeSOL  string `json:"network_fee_sol" jsonschema:"paid in SOL on top of the trade; you must hold enough SOL"`
	NetworkFeeUSDC string `json:"network_fee_usdc,omitempty"`
	ImpactUSDC     string `json:"price_impact_usdc,omitempty"`
	PlatformUSDC   string `json:"platform_fee_usdc,omitempty"`
	TotalUSDC      string `json:"total_usdc,omitempty" jsonschema:"all costs of this trade in USDC; a trade must be expected to earn more than this"`
}

func costsOutput(c trading.Costs) CostsOutput {
	out := CostsOutput{
		NetworkFeeSOL:  c.NetworkSOL(),
		NetworkFeeUSDC: usdc(c.NetworkUSDC),
		ImpactUSDC:     usdc(c.ImpactUSDC),
		PlatformUSDC:   usdc(c.PlatformUSDC),
	}
	if total, ok := c.TotalUSDC(); ok {
		out.TotalUSDC = usdc(total)
	}
	return out
}

func usdc(r *big.Rat) string {
	if r == nil {
		return ""
	}
	return r.FloatString(4)
}

// ExecuteInput is the input of execute.
type ExecuteInput struct {
	QuoteID string `json:"quote_id" jsonschema:"quote_id returned by get_quote"`
	Reason  string `json:"reason" jsonschema:"why you are making this trade: thesis and what would invalidate it"`
}

// ExecuteOutput is the output of execute.
type ExecuteOutput struct {
	TradeID  string            `json:"trade_id"`
	From     string            `json:"from"`
	To       string            `json:"to"`
	In       string            `json:"in"`
	Out      string            `json:"out"`
	Price    string            `json:"price"`
	FeeSOL   string            `json:"network_fee_sol"`
	At       time.Time         `json:"at"`
	Balances map[string]string `json:"balances"`
}

func (s *Server) execute(ctx context.Context, _ *mcp.CallToolRequest, in ExecuteInput) (*mcp.CallToolResult, ExecuteOutput, error) {
	// Take the quote out of the store up front so it can only execute once,
	// even if two calls race.
	s.mu.Lock()
	q, ok := s.quotes[in.QuoteID]
	delete(s.quotes, in.QuoteID)
	s.mu.Unlock()
	if !ok {
		return nil, ExecuteOutput{}, ErrUnknownQuote
	}

	fill, err := s.cfg.Executor.Execute(ctx, trading.Order{Quote: q, Reason: in.Reason, RunID: s.cfg.RunID})
	if err != nil {
		return nil, ExecuteOutput{}, fmt.Errorf("execute failed, no trade made: %w", err)
	}
	s.mu.Lock()
	s.tradeIDs = append(s.tradeIDs, fill.ID)
	s.mu.Unlock()
	return nil, ExecuteOutput{
		TradeID:  fill.ID,
		From:     fill.From.Symbol,
		To:       fill.To.Symbol,
		In:       venue.FormatUnits(fill.InAmount, fill.From.Decimals),
		Out:      venue.FormatUnits(fill.OutAmount, fill.To.Decimals),
		Price:    fill.Price,
		FeeSOL:   fill.Costs.NetworkSOL(),
		At:       fill.At.UTC(),
		Balances: s.cfg.Ledger.Balances(),
	}, nil
}

// BalancesOutput is the output of get_balances.
type BalancesOutput struct {
	Balances map[string]string `json:"balances"`
}

func (s *Server) getBalances(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, BalancesOutput, error) {
	return nil, BalancesOutput{Balances: s.cfg.Ledger.Balances()}, nil
}

// UpdateNarrativeInput is the input of update_narrative.
type UpdateNarrativeInput struct {
	Narrative string `json:"narrative" jsonschema:"full replacement narrative in markdown with the required headings"`
	Summary   string `json:"summary" jsonschema:"journal entry for this run: what you observed, decided and why"`
}

// UpdateNarrativeOutput is the output of update_narrative.
type UpdateNarrativeOutput struct {
	RunID    string   `json:"run_id"`
	TradeIDs []string `json:"trade_ids" jsonschema:"trades from this run linked to the journal entry"`
	Saved    bool     `json:"saved"`
}

func (s *Server) updateNarrative(_ context.Context, _ *mcp.CallToolRequest, in UpdateNarrativeInput) (*mcp.CallToolResult, UpdateNarrativeOutput, error) {
	s.mu.Lock()
	ids := append([]string(nil), s.tradeIDs...)
	s.mu.Unlock()

	entry, err := s.cfg.Memory.Update(s.cfg.RunID, in.Narrative, in.Summary, ids)
	if err != nil {
		return nil, UpdateNarrativeOutput{}, err
	}
	return nil, UpdateNarrativeOutput{RunID: entry.RunID, TradeIDs: entry.TradeIDs, Saved: true}, nil
}

// pruneLocked drops quotes past their TTL so the store doesn't grow unbounded.
func (s *Server) pruneLocked() {
	cutoff := s.now().Add(-s.cfg.QuoteTTL)
	for id, q := range s.quotes {
		if q.FetchedAt.Before(cutoff) {
			delete(s.quotes, id)
		}
	}
}

func newQuoteID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate quote id: %w", err)
	}
	return "q-" + hex.EncodeToString(b), nil
}
