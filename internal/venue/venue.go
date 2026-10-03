// Package venue defines a venue-agnostic interface for quoting (and later
// executing) token swaps, plus concrete implementations such as Jupiter.
package venue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Sentinel errors returned by venues. Callers should match with errors.Is.
var (
	// ErrUnknownToken means a symbol is not in the venue's token registry.
	ErrUnknownToken = errors.New("unknown token")
	// ErrInvalidAmount means a quote amount could not be parsed or is not positive.
	ErrInvalidAmount = errors.New("invalid amount")
	// ErrSameToken means From and To resolve to the same asset.
	ErrSameToken = errors.New("cannot swap token to itself")
)

// APIError is returned when a venue's API responds with a non-success status.
type APIError struct {
	Venue      string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Venue, e.StatusCode, e.Body)
}

// Venue is a trading venue (exchange, DEX, aggregator) that can price token
// swaps. Implementations must be safe for concurrent use.
//
// Swap execution (building/signing transactions from a Quote) will be added
// as a separate method once wallets are wired in.
type Venue interface {
	// Name identifies the venue, e.g. "jupiter".
	Name() string
	// Quote prices selling Amount of From for To.
	Quote(ctx context.Context, req QuoteRequest) (*Quote, error)
}

// QuoteRequest asks for the price of swapping Amount of From into To.
// From/To are token symbols known to the venue's registry (e.g. "USDC").
type QuoteRequest struct {
	From   string
	To     string
	Amount string // human-readable decimal, e.g. "100" or "0.25"
	// SlippageBps is the max tolerated slippage; 0 uses the venue default.
	SlippageBps uint16
}

// Quote is a venue-agnostic swap price.
type Quote struct {
	Venue string // name of the venue that produced the quote
	From  Token
	To    Token

	InAmount  *big.Int // in From's smallest units
	OutAmount *big.Int // in To's smallest units
	// MinOutAmount is the worst-case output after slippage.
	MinOutAmount *big.Int
	SlippageBps  uint16
	// PriceImpact is the fractional price impact (0.01 = 1%). Its cost is
	// already reflected in OutAmount.
	PriceImpact *big.Rat
	// PlatformFee is an integrator fee in To's smallest units, already
	// deducted from OutAmount. Nil when none is charged.
	PlatformFee    *big.Int
	PlatformFeeBps uint16
	Route          []string // human-readable hop labels, e.g. AMM names
	// FetchedAt is when the venue returned the quote; quotes go stale fast.
	FetchedAt time.Time

	// Raw is the venue's original response, needed later to build the swap
	// transaction from this exact quote.
	Raw json.RawMessage
}

// In returns the input amount as a human-readable decimal string.
func (q *Quote) In() string { return FormatUnits(q.InAmount, q.From.Decimals) }

// Out returns the expected output as a human-readable decimal string.
func (q *Quote) Out() string { return FormatUnits(q.OutAmount, q.To.Decimals) }

// MinOut returns the slippage-adjusted minimum output as a decimal string.
func (q *Quote) MinOut() string { return FormatUnits(q.MinOutAmount, q.To.Decimals) }

// Price returns units of To received per one unit of From.
func (q *Quote) Price() *big.Rat {
	if q.InAmount.Sign() == 0 {
		return new(big.Rat)
	}
	in := new(big.Rat).SetFrac(q.InAmount, pow10(q.From.Decimals))
	out := new(big.Rat).SetFrac(q.OutAmount, pow10(q.To.Decimals))
	return out.Quo(out, in)
}

// String renders the quote for logs.
func (q *Quote) String() string {
	return fmt.Sprintf("[%s] %s %s -> %s %s (min %s, price %s %s/%s, impact %s%%)",
		q.Venue, q.In(), q.From.Symbol, q.Out(), q.To.Symbol, q.MinOut(),
		q.Price().FloatString(int(q.To.Decimals)), q.To.Symbol, q.From.Symbol, q.PriceImpactPercent())
}

// PriceImpactPercent formats PriceImpact as a percentage, e.g. "0.0126".
func (q *Quote) PriceImpactPercent() string {
	if q.PriceImpact == nil {
		return "0"
	}
	p := new(big.Rat).Mul(q.PriceImpact, big.NewRat(100, 1)).FloatString(4)
	return p
}

// PlatformFeeAmount returns the platform fee as a decimal string in To units.
func (q *Quote) PlatformFeeAmount() string { return FormatUnits(q.PlatformFee, q.To.Decimals) }

// ParseUnits converts a decimal string like "1.5" into smallest units given
// the token's decimals ("1.5", 6 -> 1500000). Excess precision is an error
// rather than being silently truncated.
func ParseUnits(amount string, decimals uint8) (*big.Int, error) {
	amount = strings.TrimSpace(amount)
	whole, frac, _ := strings.Cut(amount, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > int(decimals) {
		return nil, fmt.Errorf("%w: %q has more than %d decimal places", ErrInvalidAmount, amount, decimals)
	}
	frac += strings.Repeat("0", int(decimals)-len(frac))
	n, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok || n.Sign() < 0 {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, amount)
	}
	if n.Sign() == 0 {
		return nil, fmt.Errorf("%w: must be greater than zero", ErrInvalidAmount)
	}
	return n, nil
}

// FormatUnits converts smallest units back into a decimal string.
func FormatUnits(n *big.Int, decimals uint8) string {
	if n == nil {
		return "0"
	}
	s := new(big.Rat).SetFrac(n, pow10(decimals)).FloatString(int(decimals))
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func pow10(d uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil)
}
