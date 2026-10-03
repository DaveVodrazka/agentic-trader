package trading

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"agentic-trader/internal/fsutil"
	"agentic-trader/internal/venue"
)

// Default file locations, relative to the working directory.
const (
	DefaultWalletPath = "WALLET.json"
	DefaultTradesPath = "trades.jsonl"
)

// ErrInsufficientBalance means the wallet cannot cover a trade's input.
var ErrInsufficientBalance = errors.New("insufficient balance")

// Ledger tracks wallet balances and the trade log. Safe for concurrent use.
//
// The trade log (append-only JSONL) is the source of truth. The wallet file is
// a snapshot tagged with the sequence number of the last trade it includes; on
// Open, any trades newer than the snapshot are replayed, so a crash between
// appending a trade and rewriting the snapshot is recovered automatically.
type Ledger struct {
	mu         sync.Mutex
	walletPath string
	tradesPath string
	tokens     *venue.TokenRegistry
	seq        uint64
	balances   map[string]*big.Int // symbol -> smallest units
}

// walletFile is the on-disk wallet snapshot. Balances are decimal strings.
type walletFile struct {
	LastTradeSeq uint64            `json:"last_trade_seq"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Balances     map[string]string `json:"balances"`
}

// TradeRecord is one line of the trade log. Amounts are decimal strings.
type TradeRecord struct {
	Seq      uint64    `json:"seq"`
	ID       string    `json:"id"`
	Venue    string    `json:"venue"`
	Paper    bool      `json:"paper"`
	From     string    `json:"from"`
	FromMint string    `json:"from_mint"`
	To       string    `json:"to"`
	ToMint   string    `json:"to_mint"`
	In       string    `json:"in"`
	Out      string    `json:"out"`
	Price    string    `json:"price"` // To per one From
	At       time.Time `json:"at"`
	RunID    string    `json:"run_id,omitempty"`
	Reason   string    `json:"reason,omitempty"` // agent's rationale at decision time
	// QuotedOut is the quote's expected output; differs from Out on live
	// fills by the realised slippage.
	QuotedOut string     `json:"quoted_out,omitempty"`
	Fees      *FeeRecord `json:"fees,omitempty"` // nil on trades from before fee tracking
}

// FeeRecord is the cost breakdown of one trade. NetworkSOL was deducted from
// the SOL balance; platform fee and price impact are already netted out of
// Out. USDC values are at trade time and omitted when pricing failed.
type FeeRecord struct {
	NetworkSOL     string `json:"network_sol"`
	PlatformFee    string `json:"platform_fee,omitempty"` // in To units
	PriceImpactPct string `json:"price_impact_pct,omitempty"`
	NetworkUSDC    string `json:"network_usdc,omitempty"`
	PlatformUSDC   string `json:"platform_usdc,omitempty"`
	ImpactUSDC     string `json:"impact_usdc,omitempty"`
}

func newFeeRecord(f *Fill) *FeeRecord {
	c := f.Costs
	r := &FeeRecord{NetworkSOL: c.NetworkSOL(), NetworkUSDC: ratString(c.NetworkUSDC),
		PlatformUSDC: ratString(c.PlatformUSDC), ImpactUSDC: ratString(c.ImpactUSDC)}
	if c.PlatformFee != nil {
		r.PlatformFee = venue.FormatUnits(c.PlatformFee, f.To.Decimals)
	}
	if c.PriceImpact != nil {
		r.PriceImpactPct = new(big.Rat).Mul(c.PriceImpact, big.NewRat(100, 1)).FloatString(4)
	}
	return r
}

func ratString(r *big.Rat) string {
	if r == nil {
		return ""
	}
	return r.FloatString(6)
}

// OpenLedger loads the wallet snapshot and replays any trades it is missing.
// The trade log may not exist yet; the wallet file must.
func OpenLedger(walletPath, tradesPath string, tokens *venue.TokenRegistry) (*Ledger, error) {
	l, replayed, err := loadLedger(walletPath, tradesPath, tokens)
	if err != nil {
		return nil, err
	}
	if replayed > 0 {
		if err := l.writeSnapshot(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// ReadBalances loads current balances (snapshot plus any unapplied trades)
// without writing anything, so it is safe to call while a trader is running.
func ReadBalances(walletPath, tradesPath string, tokens *venue.TokenRegistry) (map[string]string, error) {
	l, _, err := loadLedger(walletPath, tradesPath, tokens)
	if err != nil {
		return nil, err
	}
	return l.formatBalances(), nil
}

// ReadTrades returns all records in the trade log, oldest first.
func ReadTrades(tradesPath string) ([]TradeRecord, error) {
	f, err := os.Open(tradesPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open trades: %w", err)
	}
	defer f.Close()
	var recs []TradeRecord
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var rec TradeRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("trades %s line %d: %w", tradesPath, line, err)
		}
		recs = append(recs, rec)
	}
	return recs, sc.Err()
}

func loadLedger(walletPath, tradesPath string, tokens *venue.TokenRegistry) (*Ledger, int, error) {
	l := &Ledger{
		walletPath: walletPath,
		tradesPath: tradesPath,
		tokens:     tokens,
		balances:   make(map[string]*big.Int),
	}

	data, err := os.ReadFile(walletPath)
	if err != nil {
		return nil, 0, fmt.Errorf("read wallet: %w", err)
	}
	var wf walletFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, 0, fmt.Errorf("decode wallet %s: %w", walletPath, err)
	}
	for sym, amt := range wf.Balances {
		tok, err := tokens.Lookup(sym)
		if err != nil {
			return nil, 0, fmt.Errorf("wallet: %w", err)
		}
		n, err := parseBalance(amt, tok.Decimals)
		if err != nil {
			return nil, 0, fmt.Errorf("wallet %s: %w", sym, err)
		}
		l.balances[tok.Symbol] = n
	}
	l.seq = wf.LastTradeSeq

	replayed, err := l.replay()
	if err != nil {
		return nil, 0, err
	}
	return l, replayed, nil
}

// replay applies trades from the log with seq greater than the snapshot's.
func (l *Ledger) replay() (int, error) {
	f, err := os.Open(l.tradesPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open trades: %w", err)
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var rec TradeRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return 0, fmt.Errorf("trades %s line %d: %w", l.tradesPath, line, err)
		}
		if rec.Seq <= l.seq {
			continue
		}
		if rec.Seq != l.seq+1 {
			return 0, fmt.Errorf("trades %s line %d: seq gap, have %d, got %d", l.tradesPath, line, l.seq, rec.Seq)
		}
		from, in, err := l.recordAmount(rec.From, rec.In)
		if err != nil {
			return 0, fmt.Errorf("trades line %d: %w", line, err)
		}
		to, out, err := l.recordAmount(rec.To, rec.Out)
		if err != nil {
			return 0, fmt.Errorf("trades line %d: %w", line, err)
		}
		var fee *big.Int
		if rec.Fees != nil {
			if fee, err = parseBalance(rec.Fees.NetworkSOL, 9); err != nil {
				return 0, fmt.Errorf("trades line %d: network_sol: %w", line, err)
			}
		}
		if err := l.apply(from, in, to, out, fee); err != nil {
			return 0, fmt.Errorf("trades line %d: %w", line, err)
		}
		l.seq = rec.Seq
		n++
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read trades: %w", err)
	}
	return n, nil
}

func (l *Ledger) recordAmount(symbol, amount string) (venue.Token, *big.Int, error) {
	tok, err := l.tokens.Lookup(symbol)
	if err != nil {
		return venue.Token{}, nil, err
	}
	n, err := venue.ParseUnits(amount, tok.Decimals)
	return tok, n, err
}

// Balance returns the balance of symbol in smallest units (zero if unheld).
func (l *Ledger) Balance(symbol string) *big.Int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balanceLocked(strings.ToUpper(symbol))
}

func (l *Ledger) balanceLocked(symbol string) *big.Int {
	if b, ok := l.balances[symbol]; ok {
		return new(big.Int).Set(b)
	}
	return new(big.Int)
}

// Balances returns all balances as human-readable decimal strings.
func (l *Ledger) Balances() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.formatBalances()
}

// Record checks the wallet can cover the fill and its network fee (in SOL),
// applies both, appends the trade to the log, and rewrites the wallet
// snapshot. It is atomic with respect to other Record calls: a fill that
// would overdraw the wallet is rejected with ErrInsufficientBalance and
// nothing is written.
func (l *Ledger) Record(f *Fill) (*TradeRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	fee := new(big.Int).SetUint64(f.Costs.NetworkLamports)
	// Check before logging so a rejected fill writes nothing.
	if err := l.checkTrade(f.From, f.InAmount, fee); err != nil {
		return nil, err
	}

	rec := &TradeRecord{
		Seq:      l.seq + 1,
		ID:       f.ID,
		Venue:    f.Venue,
		Paper:    f.Paper,
		From:     f.From.Symbol,
		FromMint: f.From.Address,
		To:       f.To.Symbol,
		ToMint:   f.To.Address,
		In:       venue.FormatUnits(f.InAmount, f.From.Decimals),
		Out:      venue.FormatUnits(f.OutAmount, f.To.Decimals),
		Price:    f.Price,
		At:       f.At.UTC(),
		RunID:    f.RunID,
		Reason:   f.Reason,
		Fees:     newFeeRecord(f),
	}
	if f.QuotedOut != nil {
		rec.QuotedOut = venue.FormatUnits(f.QuotedOut, f.To.Decimals)
	}
	if err := l.appendTrade(rec); err != nil {
		return nil, err
	}
	// The log is now authoritative; apply cannot fail after checkTrade.
	_ = l.apply(f.From, f.InAmount, f.To, f.OutAmount, fee)
	l.seq = rec.Seq
	if err := l.writeSnapshot(); err != nil {
		// Trade is durably logged; the snapshot will be rebuilt on next Open.
		return rec, fmt.Errorf("trade %d logged but wallet snapshot failed: %w", rec.Seq, err)
	}
	return rec, nil
}

// CheckSpend reports whether the wallet holds at least amount of tok.
func (l *Ledger) CheckSpend(tok venue.Token, amount *big.Int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkSpend(tok, amount)
}

func (l *Ledger) checkSpend(tok venue.Token, amount *big.Int) error {
	have := l.balanceLocked(tok.Symbol)
	if have.Cmp(amount) < 0 {
		return fmt.Errorf("%w: need %s %s, have %s", ErrInsufficientBalance,
			venue.FormatUnits(amount, tok.Decimals), tok.Symbol, venue.FormatUnits(have, tok.Decimals))
	}
	return nil
}

// CheckTrade reports whether the wallet can pay in of from plus a network
// fee of feeLamports in SOL.
func (l *Ledger) CheckTrade(from venue.Token, in *big.Int, feeLamports *big.Int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkTrade(from, in, feeLamports)
}

func (l *Ledger) checkTrade(from venue.Token, in, fee *big.Int) error {
	if fee == nil || fee.Sign() == 0 {
		return l.checkSpend(from, in)
	}
	sol, err := l.tokens.Lookup("SOL")
	if err != nil {
		return err
	}
	if from.Symbol == sol.Symbol {
		if err := l.checkSpend(sol, new(big.Int).Add(in, fee)); err != nil {
			return fmt.Errorf("%w (including network fee)", err)
		}
		return nil
	}
	if err := l.checkSpend(from, in); err != nil {
		return err
	}
	if err := l.checkSpend(sol, fee); err != nil {
		return fmt.Errorf("%w: SOL is needed to pay the network fee", err)
	}
	return nil
}

// apply moves in of from to out of to and deducts fee lamports of SOL.
func (l *Ledger) apply(from venue.Token, in *big.Int, to venue.Token, out *big.Int, fee *big.Int) error {
	if err := l.checkTrade(from, in, fee); err != nil {
		return err
	}
	l.balances[from.Symbol] = new(big.Int).Sub(l.balanceLocked(from.Symbol), in)
	l.balances[to.Symbol] = new(big.Int).Add(l.balanceLocked(to.Symbol), out)
	if fee != nil && fee.Sign() > 0 {
		l.balances["SOL"] = new(big.Int).Sub(l.balanceLocked("SOL"), fee)
	}
	return nil
}

func (l *Ledger) appendTrade(rec *TradeRecord) error {
	line, err := fsutil.MarshalLine(rec)
	if err != nil {
		return err
	}
	return fsutil.AppendLine(l.tradesPath, line)
}

// writeSnapshot atomically replaces the wallet file.
func (l *Ledger) writeSnapshot() error {
	data, err := json.MarshalIndent(walletFile{
		LastTradeSeq: l.seq,
		UpdatedAt:    time.Now().UTC(),
		Balances:     l.formatBalances(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(l.walletPath, append(data, '\n'))
}

func (l *Ledger) formatBalances() map[string]string {
	out := make(map[string]string, len(l.balances))
	for sym, bal := range l.balances {
		tok, err := l.tokens.Lookup(sym)
		if err != nil {
			continue // only registry tokens are ever inserted
		}
		out[sym] = venue.FormatUnits(bal, tok.Decimals)
	}
	return out
}

// parseBalance is like venue.ParseUnits but allows zero.
func parseBalance(amount string, decimals uint8) (*big.Int, error) {
	if strings.Trim(strings.TrimSpace(amount), "0.") == "" && strings.TrimSpace(amount) != "" {
		return new(big.Int), nil
	}
	return venue.ParseUnits(amount, decimals)
}
