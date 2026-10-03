package trading

import (
	"context"
	"math/big"
	"sync"
	"time"

	"agentic-trader/internal/venue"
)

// LamportsPerSOL is the number of lamports in one SOL.
const LamportsPerSOL = 1_000_000_000

// FeeModel estimates the Solana network fee for executing a quote.
type FeeModel interface {
	NetworkFeeLamports(q *venue.Quote) uint64
}

// PaperFeeModel charges a fixed base fee plus a fixed priority fee. A live
// executor would read the exact fee from the confirmed transaction instead.
type PaperFeeModel struct {
	BaseLamports     uint64 // 5,000 per signature on Solana
	PriorityLamports uint64 // compute-unit price x limit; varies with congestion
}

// DefaultPaperFees approximates a single-signature swap with a modest
// priority fee (0.000105 SOL total).
var DefaultPaperFees = PaperFeeModel{BaseLamports: 5_000, PriorityLamports: 100_000}

// NetworkFeeLamports implements FeeModel.
func (m PaperFeeModel) NetworkFeeLamports(*venue.Quote) uint64 {
	return m.BaseLamports + m.PriorityLamports
}

// Pricer values token amounts in USDC.
type Pricer interface {
	ValueUSDC(ctx context.Context, tok venue.Token, amount *big.Int) (*big.Rat, error)
}

// VenuePricer prices tokens by quoting a sale into USDC on a venue.
type VenuePricer struct {
	Venue venue.Venue
}

// ValueUSDC implements Pricer.
func (p VenuePricer) ValueUSDC(ctx context.Context, tok venue.Token, amount *big.Int) (*big.Rat, error) {
	if amount == nil || amount.Sign() == 0 {
		return new(big.Rat), nil
	}
	if tok.Symbol == "USDC" {
		return ratUnits(amount, tok.Decimals), nil
	}
	q, err := p.Venue.Quote(ctx, venue.QuoteRequest{
		From: tok.Symbol, To: "USDC", Amount: venue.FormatUnits(amount, tok.Decimals),
	})
	if err != nil {
		return nil, err
	}
	return ratUnits(q.OutAmount, q.To.Decimals), nil
}

// Costs are the trading costs of one quote. Price impact and platform fees
// are already netted out of the quote's output; the network fee is paid
// separately in SOL. USDC values are nil when pricing failed.
type Costs struct {
	NetworkLamports uint64
	PlatformFee     *big.Int // in To's smallest units; nil if none
	PriceImpact     *big.Rat // fraction

	NetworkUSDC  *big.Rat
	PlatformUSDC *big.Rat
	ImpactUSDC   *big.Rat
}

// NetworkSOL returns the network fee in SOL as a decimal string.
func (c Costs) NetworkSOL() string {
	return venue.FormatUnits(new(big.Int).SetUint64(c.NetworkLamports), 9)
}

// TotalUSDC sums the priced components; ok is false if any were unpriced.
func (c Costs) TotalUSDC() (total *big.Rat, ok bool) {
	total, ok = new(big.Rat), true
	for _, v := range []*big.Rat{c.NetworkUSDC, c.PlatformUSDC, c.ImpactUSDC} {
		if v == nil {
			ok = false
			continue
		}
		total.Add(total, v)
	}
	return total, ok
}

// CostEstimator computes Costs for quotes.
type CostEstimator struct {
	Fees   FeeModel
	Pricer Pricer      // optional; without it costs are not valued in USDC
	SOL    venue.Token // fee token
}

// Estimate returns the costs of executing q. Pricing failures leave the
// corresponding USDC field nil rather than failing the estimate.
func (e CostEstimator) Estimate(ctx context.Context, q *venue.Quote) Costs {
	c := Costs{
		NetworkLamports: e.Fees.NetworkFeeLamports(q),
		PlatformFee:     q.PlatformFee,
		PriceImpact:     q.PriceImpact,
	}
	if e.Pricer == nil {
		return c
	}

	// SOL price from a 1 SOL quote: fee-sized amounts are too small to route.
	if solPrice, err := e.Pricer.ValueUSDC(ctx, e.SOL, big.NewInt(LamportsPerSOL)); err == nil {
		c.NetworkUSDC = new(big.Rat).Mul(solPrice, big.NewRat(int64(c.NetworkLamports), LamportsPerSOL))
	}

	// Value of what we receive, in USDC. Avoid a lookup when a leg is USDC.
	var outUSDC *big.Rat
	switch {
	case q.To.Symbol == "USDC":
		outUSDC = ratUnits(q.OutAmount, q.To.Decimals)
	case q.From.Symbol == "USDC":
		// Spending X USDC at some impact: received value ≈ X * (1 - impact).
		outUSDC = ratUnits(q.InAmount, q.From.Decimals)
		if q.PriceImpact != nil {
			outUSDC.Mul(outUSDC, new(big.Rat).Sub(big.NewRat(1, 1), q.PriceImpact))
		}
	default:
		if v, err := e.Pricer.ValueUSDC(ctx, q.To, q.OutAmount); err == nil {
			outUSDC = v
		}
	}
	if outUSDC == nil {
		return c
	}

	// Impact cost: output at mid price minus output received
	// = out / (1 - impact) - out.
	c.ImpactUSDC = new(big.Rat)
	if q.PriceImpact != nil && q.PriceImpact.Sign() > 0 && q.PriceImpact.Cmp(big.NewRat(1, 1)) < 0 {
		mid := new(big.Rat).Quo(outUSDC, new(big.Rat).Sub(big.NewRat(1, 1), q.PriceImpact))
		c.ImpactUSDC.Sub(mid, outUSDC)
	}

	c.PlatformUSDC = new(big.Rat)
	if q.PlatformFee != nil && q.OutAmount.Sign() > 0 {
		// Platform fee is in To units; value it at the same rate as the output.
		c.PlatformUSDC.Mul(outUSDC, new(big.Rat).SetFrac(q.PlatformFee, q.OutAmount))
	}
	return c
}

func ratUnits(n *big.Int, decimals uint8) *big.Rat {
	return new(big.Rat).SetFrac(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
}

// CachedPricer memoises another Pricer's results for TTL, keyed by token and
// amount. Used for the per-quote SOL price lookup, which would otherwise add
// an API call to every quote.
type CachedPricer struct {
	Pricer Pricer
	TTL    time.Duration

	mu    sync.Mutex
	cache map[string]cachedValue
}

type cachedValue struct {
	v  *big.Rat
	at time.Time
}

// ValueUSDC implements Pricer.
func (c *CachedPricer) ValueUSDC(ctx context.Context, tok venue.Token, amount *big.Int) (*big.Rat, error) {
	key := tok.Symbol + ":" + amount.String()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && time.Since(e.at) < c.TTL {
		c.mu.Unlock()
		return new(big.Rat).Set(e.v), nil
	}
	c.mu.Unlock()

	v, err := c.Pricer.ValueUSDC(ctx, tok, amount)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = make(map[string]cachedValue)
	}
	c.cache[key] = cachedValue{v: new(big.Rat).Set(v), at: time.Now()}
	c.mu.Unlock()
	return v, nil
}
