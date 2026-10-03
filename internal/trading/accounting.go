package trading

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

// ErrInsufficientBalance means the wallet cannot cover a trade or its fee.
var ErrInsufficientBalance = store.ErrInsufficientBalance

// Ledger is the wallet: balances and trade records, persisted in the store.
// Balance checks and updates happen in one database transaction, so
// concurrent trades (even from separate processes) cannot overdraw.
type Ledger struct {
	st     *store.Store
	tokens *venue.TokenRegistry
}

// NewLedger returns a Ledger over st.
func NewLedger(st *store.Store, tokens *venue.TokenRegistry) *Ledger {
	return &Ledger{st: st, tokens: tokens}
}

// Balance returns the balance of symbol in smallest units (zero if unheld).
func (l *Ledger) Balance(ctx context.Context, symbol string) (*big.Int, error) {
	b, err := l.st.Balances(ctx)
	if err != nil {
		return nil, err
	}
	if n, ok := b[strings.ToUpper(symbol)]; ok {
		return n, nil
	}
	return new(big.Int), nil
}

// Balances returns all non-zero balances as human-readable decimal strings.
func (l *Ledger) Balances(ctx context.Context) (map[string]string, error) {
	b, err := l.st.Balances(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(b))
	for sym, n := range b {
		tok, err := l.tokens.Lookup(sym)
		if err != nil {
			return nil, err
		}
		out[sym] = venue.FormatUnits(n, tok.Decimals)
	}
	return out, nil
}

// CheckSpend reports whether the wallet holds at least amount of tok.
func (l *Ledger) CheckSpend(ctx context.Context, tok venue.Token, amount *big.Int) error {
	return l.humanize(l.st.CheckTrade(ctx, tok.Symbol, amount, 0))
}

// CheckTrade reports whether the wallet can pay in of from plus a network
// fee of feeLamports in SOL.
func (l *Ledger) CheckTrade(ctx context.Context, from venue.Token, in *big.Int, feeLamports uint64) error {
	return l.humanize(l.st.CheckTrade(ctx, from.Symbol, in, int64(feeLamports)))
}

// Record atomically checks the wallet covers the fill and its network fee,
// applies both, and stores the trade. A fill that would overdraw the wallet
// is rejected with ErrInsufficientBalance and nothing is written.
func (l *Ledger) Record(ctx context.Context, f *Fill) (*store.Trade, error) {
	t := &store.Trade{
		ID:        f.ID,
		RunID:     f.RunID,
		At:        f.At,
		Venue:     f.Venue,
		Paper:     f.Paper,
		From:      f.From.Symbol,
		To:        f.To.Symbol,
		InAmount:  f.InAmount,
		OutAmount: f.OutAmount,
		QuotedOut: f.QuotedOut,
		Price:     f.Price,
		Reason:    f.Reason,
		Fees:      tradeFees(f.Costs),
	}
	if err := l.st.RecordTrade(ctx, t); err != nil {
		return nil, l.humanize(err)
	}
	return t, nil
}

func tradeFees(c Costs) *store.TradeFees {
	f := &store.TradeFees{
		NetworkLamports: int64(c.NetworkLamports),
		PlatformFee:     c.PlatformFee,
		NetworkUSDC:     ratString(c.NetworkUSDC),
		PlatformUSDC:    ratString(c.PlatformUSDC),
		ImpactUSDC:      ratString(c.ImpactUSDC),
	}
	if c.PriceImpact != nil {
		f.PriceImpactPct = new(big.Rat).Mul(c.PriceImpact, big.NewRat(100, 1)).FloatString(4)
	}
	return f
}

// humanize rewrites balance errors with token-formatted amounts, since they
// are shown to the agent. The result still matches ErrInsufficientBalance.
func (l *Ledger) humanize(err error) error {
	var be *store.BalanceError
	if !errors.As(err, &be) {
		return err
	}
	tok, lerr := l.tokens.Lookup(be.Symbol)
	if lerr != nil {
		return err
	}
	return fmt.Errorf("%w: need %s %s, have %s%s", ErrInsufficientBalance,
		venue.FormatUnits(big.NewInt(be.Need), tok.Decimals), tok.Symbol,
		venue.FormatUnits(big.NewInt(be.Have), tok.Decimals), be.Note())
}

func ratString(r *big.Rat) string {
	if r == nil {
		return ""
	}
	return r.FloatString(6)
}

// parseBalance is like venue.ParseUnits but allows zero.
func parseBalance(amount string, decimals uint8) (*big.Int, error) {
	if strings.Trim(strings.TrimSpace(amount), "0.") == "" && strings.TrimSpace(amount) != "" {
		return new(big.Int), nil
	}
	return venue.ParseUnits(amount, decimals)
}
