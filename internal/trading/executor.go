// Package trading turns venue quotes into executed trades and keeps the books.
package trading

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"agentic-trader/internal/venue"
)

// Errors returned by executors before a trade is attempted.
var (
	// ErrStaleQuote means the quote is older than Limits.MaxQuoteAge.
	ErrStaleQuote = errors.New("quote is stale")
	// ErrLimitExceeded means the trade breaks a configured safety limit.
	ErrLimitExceeded = errors.New("trade limit exceeded")
	// ErrMissingReason means an order was submitted without a rationale.
	ErrMissingReason = errors.New("order reason is required")
)

// Order is a quote the agent has decided to take, with its rationale.
type Order struct {
	Quote  *venue.Quote
	Reason string // why the agent is trading; required for the audit trail
	RunID  string // agent activation that placed the order
}

// Executor executes orders.
type Executor interface {
	Execute(ctx context.Context, o Order) (*Fill, error)
}

// Fill is the outcome of an executed trade. For live trades the amounts come
// from the confirmed transaction and may differ from the quote.
type Fill struct {
	ID        string // tx signature for live trades; generated for paper trades
	Venue     string
	From      venue.Token
	To        venue.Token
	InAmount  *big.Int // smallest units of From
	OutAmount *big.Int // smallest units of To
	Price     string   // To per one From
	Paper     bool
	At        time.Time
	RunID     string
	Reason    string
	QuotedOut *big.Int // the quote's expected output, for slippage tracking
	Costs     Costs
}

// Limits are hard safety rules enforced regardless of what the agent decides.
// Zero values disable the corresponding check.
type Limits struct {
	// MaxQuoteAge rejects quotes older than this.
	MaxQuoteAge time.Duration
	// MaxIn caps the input per trade, by symbol, as a decimal string
	// (e.g. {"USDC": "250"}).
	MaxIn map[string]string
	// ReserveSOL is SOL that must remain after a trade, for network fees.
	ReserveSOL string
}

// DefaultLimits are conservative defaults for paper trading.
var DefaultLimits = Limits{
	MaxQuoteAge: 30 * time.Second,
	ReserveSOL:  "0",
}

// PaperExecutor simulates execution: it fills at the quoted output amount
// without touching the chain, charges the estimated network fee in SOL, and
// records the trade in the Ledger.
type PaperExecutor struct {
	ledger *Ledger
	limits Limits
	costs  CostEstimator
	now    func() time.Time
}

var _ Executor = (*PaperExecutor)(nil)

// NewPaperExecutor returns an executor that records simulated fills in
// ledger, charging fees per costs. costs.Pricer may be nil, in which case
// fees are charged but not valued in USDC.
func NewPaperExecutor(ledger *Ledger, limits Limits, costs CostEstimator) *PaperExecutor {
	return &PaperExecutor{ledger: ledger, limits: limits, costs: costs, now: time.Now}
}

// Execute implements Executor.
func (e *PaperExecutor) Execute(ctx context.Context, o Order) (*Fill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.Reason) == "" {
		return nil, ErrMissingReason
	}
	q := o.Quote
	now := e.now()
	if err := checkLimits(e.limits, q, now); err != nil {
		return nil, err
	}
	fee := e.costs.Fees.NetworkFeeLamports(q)
	if err := e.checkReserve(q, e.costs.SOL, fee); err != nil {
		return nil, err
	}
	// Fail fast before spending API calls on pricing; Record re-checks atomically.
	if err := e.ledger.CheckTrade(q.From, q.InAmount, new(big.Int).SetUint64(fee)); err != nil {
		return nil, err
	}
	costs := e.costs.Estimate(ctx, q)

	id, err := paperID()
	if err != nil {
		return nil, err
	}
	fill := &Fill{
		ID:        id,
		Venue:     q.Venue,
		From:      q.From,
		To:        q.To,
		InAmount:  new(big.Int).Set(q.InAmount),
		OutAmount: new(big.Int).Set(q.OutAmount),
		Price:     q.Price().FloatString(int(q.To.Decimals)),
		Paper:     true,
		At:        now,
		RunID:     o.RunID,
		Reason:    strings.TrimSpace(o.Reason),
		QuotedOut: new(big.Int).Set(q.OutAmount),
		Costs:     costs,
	}
	// Record re-checks the balance atomically, so concurrent executions
	// cannot overdraw the wallet.
	if _, err := e.ledger.Record(fill); err != nil {
		return nil, err
	}
	return fill, nil
}

// checkReserve ensures a trade and its fee leave ReserveSOL behind. Checked
// here rather than in the Ledger because it is policy, not bookkeeping.
func (e *PaperExecutor) checkReserve(q *venue.Quote, sol venue.Token, feeLamports uint64) error {
	if e.limits.ReserveSOL == "" {
		return nil
	}
	reserve, err := parseBalance(e.limits.ReserveSOL, sol.Decimals)
	if err != nil {
		return fmt.Errorf("limits.ReserveSOL: %w", err)
	}
	if reserve.Sign() == 0 {
		return nil
	}
	need := new(big.Int).Add(reserve, new(big.Int).SetUint64(feeLamports))
	if q.From.Symbol == sol.Symbol {
		need.Add(need, q.InAmount)
	}
	if err := e.ledger.CheckSpend(sol, need); err != nil {
		return fmt.Errorf("%w: must keep %s SOL for fees: %w", ErrLimitExceeded, e.limits.ReserveSOL, err)
	}
	return nil
}

// checkLimits applies the venue-independent safety rules to q.
func checkLimits(l Limits, q *venue.Quote, now time.Time) error {
	if l.MaxQuoteAge > 0 {
		if age := now.Sub(q.FetchedAt); age > l.MaxQuoteAge {
			return fmt.Errorf("%w: %s old, max %s", ErrStaleQuote, age.Round(time.Millisecond), l.MaxQuoteAge)
		}
	}
	if maxStr, ok := l.MaxIn[q.From.Symbol]; ok {
		limit, err := venue.ParseUnits(maxStr, q.From.Decimals)
		if err != nil {
			return fmt.Errorf("limits.MaxIn[%s]: %w", q.From.Symbol, err)
		}
		if q.InAmount.Cmp(limit) > 0 {
			return fmt.Errorf("%w: %s %s exceeds max %s", ErrLimitExceeded, q.In(), q.From.Symbol, maxStr)
		}
	}
	return nil
}

func paperID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate trade id: %w", err)
	}
	return "paper-" + hex.EncodeToString(b), nil
}
