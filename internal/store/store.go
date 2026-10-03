// Package store persists all trading state in a single SQLite database:
// deposits, balances, trades, agent runs, journal, narratives, quotes,
// prices and portfolio snapshots.
//
// Conventions, chosen so the data can be read directly by a frontend:
//   - token amounts are INTEGER in the token's smallest units; the tokens
//     table has the decimals to format them
//   - USDC values and prices are TEXT decimals (display data, not ledger)
//   - timestamps are TEXT, UTC, fixed-width RFC 3339 with microseconds, so
//     they sort lexicographically
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DefaultPath is the database location relative to the working directory.
const DefaultPath = "trader.db"

// SOL pays network fees.
const feeSymbol = "SOL"

// ErrInsufficientBalance means the wallet cannot cover a trade or its fee.
var ErrInsufficientBalance = errors.New("insufficient balance")

// ErrNotInitialized means the database has no deposits yet.
var ErrNotInitialized = errors.New("database not initialized; run 'make init'")

const timeLayout = "2006-01-02T15:04:05.000000Z"

func ts(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(timeLayout, s)
	return t
}

// Store is a handle to the database. Safe for concurrent use, including from
// multiple processes: writes run in IMMEDIATE transactions.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the
// schema.
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- tokens ----------------------------------------------------------------

// Token is a known asset.
type Token struct {
	Symbol   string
	Mint     string
	Decimals uint8
}

// UpsertTokens records token metadata (symbol, mint, decimals).
func (s *Store) UpsertTokens(ctx context.Context, tokens []Token) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, t := range tokens {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO tokens(symbol, mint, decimals) VALUES(?,?,?)
				 ON CONFLICT(symbol) DO UPDATE SET mint=excluded.mint, decimals=excluded.decimals`,
				t.Symbol, t.Mint, t.Decimals); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- deposits & balances ---------------------------------------------------

// Deposit is capital added to the wallet. The first deposit marks the start.
type Deposit struct {
	At        time.Time
	Symbol    string
	Amount    *big.Int
	ValueUSDC string // value at deposit time; the PnL baseline
	Note      string
}

// AddDeposit records a deposit and credits the balance.
func (s *Store) AddDeposit(ctx context.Context, d Deposit) error {
	amt, err := toInt64(d.Amount)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deposits(at, symbol, amount, value_usdc, note) VALUES(?,?,?,?,?)`,
			ts(d.At), d.Symbol, amt, d.ValueUSDC, d.Note); err != nil {
			return err
		}
		return addBalance(ctx, tx, d.Symbol, amt, d.At)
	})
}

// Deposits returns all deposits, oldest first.
func (s *Store) Deposits(ctx context.Context) ([]Deposit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, symbol, amount, value_usdc, note FROM deposits ORDER BY at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deposit
	for rows.Next() {
		var d Deposit
		var at string
		var amt int64
		if err := rows.Scan(&at, &d.Symbol, &amt, &d.ValueUSDC, &d.Note); err != nil {
			return nil, err
		}
		d.At, d.Amount = parseTS(at), big.NewInt(amt)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Initialized reports whether any deposit has been made.
func (s *Store) Initialized(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM deposits`).Scan(&n)
	return n > 0, err
}

// Balances returns all non-zero balances in smallest units.
func (s *Store) Balances(ctx context.Context) (map[string]*big.Int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT symbol, amount FROM balances WHERE amount != 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*big.Int)
	for rows.Next() {
		var sym string
		var amt int64
		if err := rows.Scan(&sym, &amt); err != nil {
			return nil, err
		}
		out[sym] = big.NewInt(amt)
	}
	return out, rows.Err()
}

func balance(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, symbol string) (int64, error) {
	var amt int64
	err := q.QueryRowContext(ctx, `SELECT amount FROM balances WHERE symbol=?`, symbol).Scan(&amt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return amt, err
}

// addBalance applies delta to symbol's balance. Not an upsert: SQLite checks
// the CHECK(amount >= 0) constraint on the would-be-inserted row (the bare
// delta) before resolving the conflict, which rejects every debit.
func addBalance(ctx context.Context, tx *sql.Tx, symbol string, delta int64, at time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE balances SET amount = amount + ?, updated_at = ? WHERE symbol = ?`,
		delta, ts(at), symbol)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO balances(symbol, amount, updated_at) VALUES(?,?,?)`, symbol, delta, ts(at))
	return err
}

// ---- trades ----------------------------------------------------------------

// Trade is an executed swap with its costs.
type Trade struct {
	ID        string
	Seq       int64 // assigned by RecordTrade
	RunID     string
	At        time.Time
	Venue     string
	Paper     bool
	From      string
	To        string
	InAmount  *big.Int
	OutAmount *big.Int
	QuotedOut *big.Int // nil if unknown
	Price     string   // To per one From
	Reason    string
	// ActivationID is the strategy activation that placed the trade (0 if
	// the agent traded directly).
	ActivationID int64

	// Fees is nil for trades from before fee tracking.
	Fees *TradeFees
}

// TradeFees is the cost breakdown of a trade. The network fee is deducted
// from SOL; platform fee and price impact are already netted out of
// OutAmount. USDC values are at trade time; empty when pricing failed.
type TradeFees struct {
	NetworkLamports int64
	PlatformFee     *big.Int // To units; nil if none
	PriceImpactPct  string
	NetworkUSDC     string
	PlatformUSDC    string
	ImpactUSDC      string
}

// RecordTrade atomically checks the wallet covers the trade and its network
// fee, applies both to balances, and inserts the trade. On
// ErrInsufficientBalance nothing is written. t.Seq is set on success.
func (s *Store) RecordTrade(ctx context.Context, t *Trade) error {
	in, err := toInt64(t.InAmount)
	if err != nil {
		return err
	}
	out, err := toInt64(t.OutAmount)
	if err != nil {
		return err
	}
	var fee int64
	if t.Fees != nil {
		fee = t.Fees.NetworkLamports
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkTrade(ctx, tx, t.From, in, fee); err != nil {
			return err
		}
		if t.RunID != "" {
			if err := ensureRun(ctx, tx, t.RunID, t.At); err != nil {
				return err
			}
		}
		var seq int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0)+1 FROM trades`).Scan(&seq); err != nil {
			return err
		}
		f := t.Fees
		tracked := f != nil
		if f == nil {
			f = &TradeFees{}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO trades(
				id, seq, run_id, at, venue, paper, from_symbol, to_symbol, in_amount, out_amount, quoted_out,
				price, reason, fees_tracked, network_fee_lamports, platform_fee, price_impact_pct,
				network_fee_usdc, platform_fee_usdc, impact_usdc, activation_id)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, seq, nullStr(t.RunID), ts(t.At), t.Venue, t.Paper, t.From, t.To, in, out, nullInt(t.QuotedOut),
			t.Price, t.Reason, tracked, f.NetworkLamports, nullInt(f.PlatformFee), nullStr(f.PriceImpactPct),
			nullStr(f.NetworkUSDC), nullStr(f.PlatformUSDC), nullStr(f.ImpactUSDC), nullID(t.ActivationID)); err != nil {
			return err
		}
		if err := addBalance(ctx, tx, t.From, -in, t.At); err != nil {
			return err
		}
		if err := addBalance(ctx, tx, t.To, out, t.At); err != nil {
			return err
		}
		if fee > 0 {
			if err := addBalance(ctx, tx, feeSymbol, -fee, t.At); err != nil {
				return err
			}
		}
		t.Seq = seq
		return nil
	})
}

// CheckTrade reports whether the wallet can pay in of from plus fee lamports
// of SOL, without writing.
func (s *Store) CheckTrade(ctx context.Context, from string, in *big.Int, feeLamports int64) error {
	n, err := toInt64(in)
	if err != nil {
		return err
	}
	return checkTrade(ctx, s.db, from, n, feeLamports)
}

func checkTrade(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, from string, in, fee int64) error {
	have, err := balance(ctx, q, from)
	if err != nil {
		return err
	}
	need := in
	if from == feeSymbol {
		need += fee
	}
	if have < need {
		return &BalanceError{Symbol: from, Need: need, Have: have, IncludesFee: from == feeSymbol && fee > 0}
	}
	if from != feeSymbol && fee > 0 {
		sol, err := balance(ctx, q, feeSymbol)
		if err != nil {
			return err
		}
		if sol < fee {
			return &BalanceError{Symbol: feeSymbol, Need: fee, Have: sol, ForFee: true}
		}
	}
	return nil
}

// BalanceError details an ErrInsufficientBalance. Amounts are smallest units.
type BalanceError struct {
	Symbol      string
	Need, Have  int64
	IncludesFee bool // Need includes the network fee (spending SOL)
	ForFee      bool // the shortfall is SOL for the network fee
}

func (e *BalanceError) Error() string {
	return fmt.Sprintf("%s: %s need %d, have %d (smallest units)%s", ErrInsufficientBalance, e.Symbol, e.Need, e.Have, e.Note())
}

// Note explains fee involvement, or is empty.
func (e *BalanceError) Note() string {
	switch {
	case e.ForFee:
		return "; SOL is needed to pay the network fee"
	case e.IncludesFee:
		return " (including network fee)"
	}
	return ""
}

func (e *BalanceError) Unwrap() error { return ErrInsufficientBalance }

// Trades returns all trades, oldest first.
func (s *Store) Trades(ctx context.Context) ([]Trade, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, seq, coalesce(run_id,''), at, venue, paper, from_symbol, to_symbol,
			in_amount, out_amount, quoted_out, price, reason, fees_tracked, network_fee_lamports, platform_fee,
			coalesce(price_impact_pct,''), coalesce(network_fee_usdc,''), coalesce(platform_fee_usdc,''), coalesce(impact_usdc,''),
			coalesce(activation_id, 0)
		FROM trades ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		var t Trade
		var at string
		var in, outAmt int64
		var quoted, platform sql.NullInt64
		var tracked bool
		var f TradeFees
		if err := rows.Scan(&t.ID, &t.Seq, &t.RunID, &at, &t.Venue, &t.Paper, &t.From, &t.To, &in, &outAmt, &quoted,
			&t.Price, &t.Reason, &tracked, &f.NetworkLamports, &platform, &f.PriceImpactPct, &f.NetworkUSDC,
			&f.PlatformUSDC, &f.ImpactUSDC, &t.ActivationID); err != nil {
			return nil, err
		}
		t.At, t.InAmount, t.OutAmount = parseTS(at), big.NewInt(in), big.NewInt(outAmt)
		if quoted.Valid {
			t.QuotedOut = big.NewInt(quoted.Int64)
		}
		if platform.Valid {
			f.PlatformFee = big.NewInt(platform.Int64)
		}
		if tracked {
			t.Fees = &f
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- runs ------------------------------------------------------------------

// Run is one agent activation.
type Run struct {
	ID         string
	StartedAt  time.Time
	FinishedAt time.Time // zero while running
	ExitCode   *int
	Prompt     string
	Report     string // the agent's output
	LogPath    string
}

// BeginRun records the start of a run. Idempotent.
func (s *Store) BeginRun(ctx context.Context, id, prompt string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id, started_at, prompt) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING`,
		id, ts(at), prompt)
	return err
}

// EndRun records the end of a run, creating it if BeginRun never ran.
func (s *Store) EndRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id, started_at, finished_at, exit_code, prompt, report, log_path) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET finished_at=excluded.finished_at, exit_code=excluded.exit_code,
		   report=excluded.report, log_path=excluded.log_path`,
		r.ID, ts(r.StartedAt), ts(r.FinishedAt), r.ExitCode, nullStr(r.Prompt), nullStr(r.Report), nullStr(r.LogPath))
	return err
}

// CountRuns returns the number of recorded runs.
func (s *Store) CountRuns(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs`).Scan(&n)
	return n, err
}

// Runs returns the last limit runs, newest first.
func (s *Store) Runs(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, started_at, coalesce(finished_at,''), exit_code,
			coalesce(prompt,''), coalesce(report,''), coalesce(log_path,'')
		FROM runs ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started, finished string
		var code sql.NullInt64
		if err := rows.Scan(&r.ID, &started, &finished, &code, &r.Prompt, &r.Report, &r.LogPath); err != nil {
			return nil, err
		}
		r.StartedAt = parseTS(started)
		if finished != "" {
			r.FinishedAt = parseTS(finished)
		}
		if code.Valid {
			c := int(code.Int64)
			r.ExitCode = &c
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- journal & narratives --------------------------------------------------

// JournalEntry is the agent's summary of one run.
type JournalEntry struct {
	RunID    string
	At       time.Time
	Summary  string
	TradeIDs []string // trades made in the same run
}

// Narrative is one version of the agent's working memory.
type Narrative struct {
	RunID string
	At    time.Time
	Body  string
}

// SaveNarrative stores a journal entry and a new narrative version together.
func (s *Store) SaveNarrative(ctx context.Context, runID, body, summary string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ensureRun(ctx, tx, runID, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO journal(run_id, at, summary) VALUES(?,?,?)`,
			runID, ts(at), summary); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO narratives(run_id, at, body) VALUES(?,?,?)`, runID, ts(at), body)
		return err
	})
}

// AddJournal stores a journal entry on its own (used by the legacy import).
func (s *Store) AddJournal(ctx context.Context, runID, summary string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ensureRun(ctx, tx, runID, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO journal(run_id, at, summary) VALUES(?,?,?)`, runID, ts(at), summary)
		return err
	})
}

// AddNarrative stores a narrative version on its own (used by the legacy import).
func (s *Store) AddNarrative(ctx context.Context, runID, body string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ensureRun(ctx, tx, runID, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO narratives(run_id, at, body) VALUES(?,?,?)`, runID, ts(at), body)
		return err
	})
}

// ensureRun creates a placeholder run row so foreign keys hold for runs that
// were not started via BeginRun (e.g. interactive sessions).
func ensureRun(ctx context.Context, tx *sql.Tx, runID string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO runs(id, started_at) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, runID, ts(at))
	return err
}

// LatestNarrative returns the most recent narrative, or ok=false if none.
func (s *Store) LatestNarrative(ctx context.Context) (n Narrative, ok bool, err error) {
	var at string
	err = s.db.QueryRowContext(ctx, `SELECT run_id, at, body FROM narratives ORDER BY at DESC, id DESC LIMIT 1`).
		Scan(&n.RunID, &at, &n.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return n, false, nil
	}
	n.At = parseTS(at)
	return n, err == nil, err
}

// Journal returns the last limit journal entries, oldest first (limit <= 0
// for all), each with the IDs of trades made in its run.
func (s *Store) Journal(ctx context.Context, limit int) ([]JournalEntry, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT j.run_id, j.at, j.summary,
			coalesce((SELECT group_concat(t.id, ',') FROM (SELECT id FROM trades WHERE run_id=j.run_id ORDER BY seq) t), '')
		FROM (SELECT * FROM journal ORDER BY at DESC, id DESC LIMIT ?) j ORDER BY j.at, j.id`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var at, ids string
		if err := rows.Scan(&e.RunID, &at, &e.Summary, &ids); err != nil {
			return nil, err
		}
		e.At = parseTS(at)
		if ids != "" {
			e.TradeIDs = splitComma(ids)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- quotes, prices, snapshots ---------------------------------------------

// Quote is a price the agent was shown.
type Quote struct {
	QuoteID        string
	RunID          string
	At             time.Time
	Venue          string
	From, To       string
	InAmount       *big.Int
	OutAmount      *big.Int
	MinOut         *big.Int
	Price          string // To per one From
	PriceImpactPct string
	Route          string // JSON array of hop labels
}

// RecordQuote stores a quote.
func (s *Store) RecordQuote(ctx context.Context, q Quote) error {
	in, err := toInt64(q.InAmount)
	if err != nil {
		return err
	}
	out, err := toInt64(q.OutAmount)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if q.RunID != "" {
			if err := ensureRun(ctx, tx, q.RunID, q.At); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO quotes(quote_id, run_id, at, venue, from_symbol, to_symbol,
				in_amount, out_amount, min_out, price, price_impact_pct, route) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			q.QuoteID, nullStr(q.RunID), ts(q.At), q.Venue, q.From, q.To, in, out, nullInt(q.MinOut),
			q.Price, q.PriceImpactPct, q.Route)
		return err
	})
}

// MarkQuoteExecuted links a quote to the trade that executed it.
func (s *Store) MarkQuoteExecuted(ctx context.Context, quoteID, tradeID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE quotes SET trade_id=? WHERE quote_id=?`, tradeID, quoteID)
	return err
}

// QuoteTrade returns the trade ID a recorded quote was executed as ("" if
// not executed); found is false if the quote was never recorded.
func (s *Store) QuoteTrade(ctx context.Context, quoteID string) (tradeID string, found bool, err error) {
	var id sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT trade_id FROM quotes WHERE quote_id=?`, quoteID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id.String, err == nil, err
}

// Price is a token's USDC price at a point in time.
type Price struct {
	At     time.Time
	Symbol string
	USDC   string // USDC per one token
	Source string // "quote" or "snapshot"
}

// RecordPrices stores price observations.
func (s *Store) RecordPrices(ctx context.Context, prices []Price) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return insertPrices(ctx, tx, prices) })
}

func insertPrices(ctx context.Context, tx *sql.Tx, prices []Price) error {
	for _, p := range prices {
		if _, err := tx.ExecContext(ctx, `INSERT INTO prices(at, symbol, price_usdc, source) VALUES(?,?,?,?)`,
			ts(p.At), p.Symbol, p.USDC, p.Source); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot kinds.
const (
	KindInitial = "initial" // the starting wallet
	KindRun     = "run"     // after an agent run
	KindManual  = "manual"  // on request
)

// Snapshot is an immutable PnL record: the portfolio and every PnL figure at
// one moment. Decimal fields are strings; empty means NULL (not available).
type Snapshot struct {
	ID       int64 // assigned by RecordSnapshot
	TakenAt  time.Time
	Kind     string
	RunID    string
	Complete bool

	ValueUSDC      string
	DepositsUSDC   string
	PnLUSDC        string
	ReturnPct      string
	APRPct         string
	APYPct         string
	GrossPnLUSDC   string
	CostsUSDC      string
	NetworkFeesSOL string
	CashPct        string
	TradeCount     int
	RunCount       int

	Holdings []SnapshotHolding // largest value first
}

// SnapshotHolding is one token in a snapshot.
type SnapshotHolding struct {
	Symbol    string
	Amount    *big.Int // smallest units
	PriceUSDC string   // empty if unpriced
	ValueUSDC string
	WeightPct string
	Error     string
}

// RecordSnapshot stores snap with its holdings, plus a price observation per
// priced non-USDC holding, and sets snap.ID.
func (s *Store) RecordSnapshot(ctx context.Context, snap *Snapshot) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if snap.RunID != "" {
			if err := ensureRun(ctx, tx, snap.RunID, snap.TakenAt); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO snapshots(taken_at, kind, run_id, complete, value_usdc, deposits_usdc,
				pnl_usdc, return_pct, apr_pct, apy_pct, gross_pnl_usdc, costs_usdc, network_fees_sol, cash_pct,
				trade_count, run_count) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ts(snap.TakenAt), snap.Kind, nullStr(snap.RunID), snap.Complete, snap.ValueUSDC, snap.DepositsUSDC,
			snap.PnLUSDC, snap.ReturnPct, nullStr(snap.APRPct), nullStr(snap.APYPct), snap.GrossPnLUSDC,
			snap.CostsUSDC, snap.NetworkFeesSOL, snap.CashPct, snap.TradeCount, snap.RunCount)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		var prices []Price
		for _, h := range snap.Holdings {
			amt, err := toInt64(h.Amount)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_holdings(snapshot_id, symbol, amount, price_usdc,
					value_usdc, weight_pct, error) VALUES(?,?,?,?,?,?,?)`,
				id, h.Symbol, amt, nullStr(h.PriceUSDC), nullStr(h.ValueUSDC), nullStr(h.WeightPct), nullStr(h.Error)); err != nil {
				return err
			}
			if h.PriceUSDC != "" && h.Symbol != "USDC" {
				prices = append(prices, Price{At: snap.TakenAt, Symbol: h.Symbol, USDC: h.PriceUSDC, Source: "snapshot"})
			}
		}
		if err := insertPrices(ctx, tx, prices); err != nil {
			return err
		}
		snap.ID = id
		return nil
	})
}

const snapshotCols = `id, taken_at, kind, coalesce(run_id,''), complete, value_usdc, deposits_usdc, pnl_usdc, return_pct,
	coalesce(apr_pct,''), coalesce(apy_pct,''), gross_pnl_usdc, costs_usdc, network_fees_sol, cash_pct, trade_count, run_count`

func scanSnapshot(row interface{ Scan(...any) error }) (Snapshot, error) {
	var sn Snapshot
	var at string
	err := row.Scan(&sn.ID, &at, &sn.Kind, &sn.RunID, &sn.Complete, &sn.ValueUSDC, &sn.DepositsUSDC, &sn.PnLUSDC,
		&sn.ReturnPct, &sn.APRPct, &sn.APYPct, &sn.GrossPnLUSDC, &sn.CostsUSDC, &sn.NetworkFeesSOL, &sn.CashPct,
		&sn.TradeCount, &sn.RunCount)
	sn.TakenAt = parseTS(at)
	return sn, err
}

// kindFilter returns a SQL condition and args restricting to kinds (all if
// none given).
func kindFilter(kinds []string) (string, []any) {
	if len(kinds) == 0 {
		return "1=1", nil
	}
	cond := "kind IN (?" + strings.Repeat(",?", len(kinds)-1) + ")"
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	return cond, args
}

// GetSnapshot returns snapshot id with its holdings; ok is false if absent.
func (s *Store) GetSnapshot(ctx context.Context, id int64) (snap Snapshot, ok bool, err error) {
	snap, err = scanSnapshot(s.db.QueryRowContext(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return snap, false, nil
	}
	if err != nil {
		return snap, false, err
	}
	snap.Holdings, err = s.snapshotHoldings(ctx, id)
	return snap, err == nil, err
}

// LatestSnapshot returns the newest snapshot of the given kinds (any kind if
// none) with its holdings.
func (s *Store) LatestSnapshot(ctx context.Context, kinds ...string) (Snapshot, bool, error) {
	cond, args := kindFilter(kinds)
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM snapshots WHERE `+cond+` ORDER BY id DESC LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	return s.GetSnapshot(ctx, id)
}

// AdjacentSnapshots returns the IDs of the snapshots before and after id
// among the given kinds (any kind if none); 0 means there is none.
func (s *Store) AdjacentSnapshots(ctx context.Context, id int64, kinds ...string) (prev, next int64, err error) {
	cond, args := kindFilter(kinds)
	err = s.db.QueryRowContext(ctx, `SELECT
			coalesce((SELECT max(id) FROM snapshots WHERE id < ? AND `+cond+`), 0),
			coalesce((SELECT min(id) FROM snapshots WHERE id > ? AND `+cond+`), 0)`,
		append(append([]any{id}, args...), append([]any{id}, args...)...)...).Scan(&prev, &next)
	return prev, next, err
}

// ListSnapshots returns snapshot headers (no holdings), newest first, of the
// given kinds, with id < beforeID (0 for the newest page).
func (s *Store) ListSnapshots(ctx context.Context, beforeID int64, limit int, kinds ...string) ([]Snapshot, error) {
	cond, args := kindFilter(kinds)
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE id < ? AND `+cond+
		` ORDER BY id DESC LIMIT ?`, append(append([]any{beforeID}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		sn, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

func (s *Store) snapshotHoldings(ctx context.Context, id int64) ([]SnapshotHolding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT symbol, amount, coalesce(price_usdc,''), coalesce(value_usdc,''),
			coalesce(weight_pct,''), coalesce(error,'')
		FROM snapshot_holdings WHERE snapshot_id=? ORDER BY CAST(value_usdc AS REAL) DESC, symbol`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotHolding
	for rows.Next() {
		var h SnapshotHolding
		var amt int64
		if err := rows.Scan(&h.Symbol, &amt, &h.PriceUSDC, &h.ValueUSDC, &h.WeightPct, &h.Error); err != nil {
			return nil, err
		}
		h.Amount = big.NewInt(amt)
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- candles -------------------------------------------------------------

// Candle is an OHLC price bar in USD.
type Candle struct {
	Symbol   string
	Interval string // "5m", "1h", "1d"
	Start    time.Time
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Source   string
}

// CandleWrite says how UpsertCandles treats bars that already exist.
type CandleWrite int

const (
	// InsertMissing keeps every existing bar.
	InsertMissing CandleWrite = iota
	// ReplaceSameSource refreshes existing bars from the same source (e.g.
	// a provider's previously in-progress bar) but keeps bars from others,
	// such as our own ticks.
	ReplaceSameSource
	// ReplaceAll overwrites existing bars.
	ReplaceAll
)

// UpsertCandles stores bars (e.g. from a backfill); mode decides what
// happens to existing bars with the same symbol, interval and start.
func (s *Store) UpsertCandles(ctx context.Context, candles []Candle, mode CandleWrite) error {
	q := `INSERT INTO candles(symbol, interval, start, open, high, low, close, source) VALUES(?,?,?,?,?,?,?,?)`
	switch mode {
	case InsertMissing:
		q += ` ON CONFLICT(symbol, interval, start) DO NOTHING`
	case ReplaceSameSource:
		q += ` ON CONFLICT(symbol, interval, start) DO UPDATE SET open=excluded.open, high=excluded.high,
			low=excluded.low, close=excluded.close WHERE candles.source = excluded.source`
	case ReplaceAll:
		q += ` ON CONFLICT(symbol, interval, start) DO UPDATE SET open=excluded.open, high=excluded.high,
			low=excluded.low, close=excluded.close, source=excluded.source`
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, c := range candles {
			if _, err := tx.ExecContext(ctx, q, c.Symbol, c.Interval, ts(c.Start), c.Open, c.High, c.Low, c.Close, c.Source); err != nil {
				return err
			}
		}
		return nil
	})
}

// CandleStarts returns the start times of stored bars at or after from,
// oldest first.
func (s *Store) CandleStarts(ctx context.Context, symbol, interval string, from time.Time) ([]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT start FROM candles WHERE symbol=? AND interval=? AND start >= ? ORDER BY start`,
		symbol, interval, ts(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return nil, err
		}
		out = append(out, parseTS(st))
	}
	return out, rows.Err()
}

// DeleteCandle removes one bar.
func (s *Store) DeleteCandle(ctx context.Context, symbol, interval string, start time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM candles WHERE symbol=? AND interval=? AND start=?`, symbol, interval, ts(start))
	return err
}

// EarliestCandle returns the start of the oldest stored bar.
func (s *Store) EarliestCandle(ctx context.Context, symbol, interval string) (time.Time, bool, error) {
	var st sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT min(start) FROM candles WHERE symbol=? AND interval=?`, symbol, interval).Scan(&st)
	if err != nil || !st.Valid {
		return time.Time{}, false, err
	}
	return parseTS(st.String), true, nil
}

// BackfillState is the backfill bookkeeping for one symbol and interval.
type BackfillState struct {
	Symbol         string
	Interval       string
	Provider       string
	Pool           string
	PoolName       string
	Earliest       time.Time // oldest bar the provider has served (zero if unknown)
	CheckedThrough time.Time // newest bar already requested (zero if never)
}

// GetBackfillState returns the state for symbol/interval; ok is false if
// there is none.
func (s *Store) GetBackfillState(ctx context.Context, symbol, interval string) (BackfillState, bool, error) {
	b := BackfillState{Symbol: symbol, Interval: interval}
	var earliest, checked sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT provider, pool, pool_name, earliest, checked_through FROM backfill_state
		WHERE symbol=? AND interval=?`, symbol, interval).Scan(&b.Provider, &b.Pool, &b.PoolName, &earliest, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, false, err
	}
	if earliest.Valid {
		b.Earliest = parseTS(earliest.String)
	}
	if checked.Valid {
		b.CheckedThrough = parseTS(checked.String)
	}
	return b, true, nil
}

// SaveBackfillState upserts backfill state.
func (s *Store) SaveBackfillState(ctx context.Context, b BackfillState, now time.Time) error {
	opt := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return ts(t)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO backfill_state(symbol, interval, provider, pool, pool_name, earliest,
			checked_through, updated_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(symbol, interval) DO UPDATE SET provider=excluded.provider, pool=excluded.pool,
			pool_name=excluded.pool_name, earliest=excluded.earliest, checked_through=excluded.checked_through,
			updated_at=excluded.updated_at`,
		b.Symbol, b.Interval, b.Provider, b.Pool, b.PoolName, opt(b.Earliest), opt(b.CheckedThrough), ts(now))
	return err
}

// AddPriceTicks folds price observations into the bars of each interval
// (open on first tick, high/low extended, close updated) and records them in
// the prices table.
func (s *Store) AddPriceTicks(ctx context.Context, at time.Time, prices map[string]float64, intervals map[string]time.Duration, source string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for sym, p := range prices {
			for name, d := range intervals {
				start := at.UTC().Truncate(d)
				if _, err := tx.ExecContext(ctx, `INSERT INTO candles(symbol, interval, start, open, high, low, close, source)
						VALUES(?,?,?,?,?,?,?,?)
						ON CONFLICT(symbol, interval, start) DO UPDATE SET high=max(high, excluded.high),
						low=min(low, excluded.low), close=excluded.close`,
					sym, name, ts(start), p, p, p, p, source); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO prices(at, symbol, price_usdc, source) VALUES(?,?,?,?)`,
				ts(at), sym, fmt.Sprintf("%.12g", p), source); err != nil {
				return err
			}
		}
		return nil
	})
}

// Candles returns bars for symbol and interval whose start is in [from, to),
// oldest first.
func (s *Store) Candles(ctx context.Context, symbol, interval string, from, to time.Time) ([]Candle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT start, open, high, low, close, source FROM candles
		WHERE symbol=? AND interval=? AND start >= ? AND start < ? ORDER BY start`,
		symbol, interval, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candle
	for rows.Next() {
		c := Candle{Symbol: symbol, Interval: interval}
		var start string
		if err := rows.Scan(&start, &c.Open, &c.High, &c.Low, &c.Close, &c.Source); err != nil {
			return nil, err
		}
		c.Start = parseTS(start)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- strategies ------------------------------------------------------------

// Activation statuses.
const (
	StatusActive = "active"
	StatusHalted = "halted" // stopped by the risk layer; needs a new activation
	StatusEnded  = "ended"
)

// Activation is a strategy put in charge of the portfolio.
type Activation struct {
	ID         int64
	Strategy   string
	Params     string // JSON
	Reason     string
	SetBy      string // "agent" or "user"
	RunID      string
	StartedAt  time.Time
	EndedAt    time.Time // zero while live
	Status     string
	HaltReason string
	State      string // JSON
}

// LiveStrategy returns the current (not ended) activation, if any.
func (s *Store) LiveStrategy(ctx context.Context) (Activation, bool, error) {
	a, err := scanActivation(s.db.QueryRowContext(ctx, `SELECT `+activationCols+` FROM strategy_activations WHERE ended_at IS NULL`))
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	return a, err == nil, err
}

// StartStrategy ends the live activation (if any) and starts a new one,
// setting a.ID.
func (s *Store) StartStrategy(ctx context.Context, a *Activation) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE strategy_activations SET ended_at=?, status=? WHERE ended_at IS NULL`,
			ts(a.StartedAt), StatusEnded); err != nil {
			return err
		}
		if a.RunID != "" {
			if err := ensureRun(ctx, tx, a.RunID, a.StartedAt); err != nil {
				return err
			}
		}
		if a.State == "" {
			a.State = "{}"
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO strategy_activations(strategy, params, reason, set_by, run_id,
				started_at, status, state) VALUES(?,?,?,?,?,?,?,?)`,
			a.Strategy, a.Params, a.Reason, a.SetBy, nullStr(a.RunID), ts(a.StartedAt), StatusActive, a.State)
		if err != nil {
			return err
		}
		a.ID, err = res.LastInsertId()
		a.Status = StatusActive
		return err
	})
}

// SaveStrategyState persists an activation's state; if haltReason is set the
// activation is halted.
func (s *Store) SaveStrategyState(ctx context.Context, id int64, state, haltReason string) error {
	if haltReason != "" {
		_, err := s.db.ExecContext(ctx, `UPDATE strategy_activations SET state=?, status=?, halt_reason=? WHERE id=?`,
			state, StatusHalted, haltReason, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE strategy_activations SET state=? WHERE id=?`, state, id)
	return err
}

// Activations returns activations newest first (limit <= 0 for all).
func (s *Store) Activations(ctx context.Context, limit int) ([]Activation, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+activationCols+` FROM strategy_activations ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activation
	for rows.Next() {
		a, err := scanActivation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const activationCols = `id, strategy, params, reason, set_by, coalesce(run_id,''), started_at, coalesce(ended_at,''),
	status, coalesce(halt_reason,''), state`

func scanActivation(row interface{ Scan(...any) error }) (Activation, error) {
	var a Activation
	var started, ended string
	err := row.Scan(&a.ID, &a.Strategy, &a.Params, &a.Reason, &a.SetBy, &a.RunID, &started, &ended, &a.Status,
		&a.HaltReason, &a.State)
	a.StartedAt = parseTS(started)
	if ended != "" {
		a.EndedAt = parseTS(ended)
	}
	return a, err
}

// Tick is one evaluation of the live strategy.
type Tick struct {
	ActivationID int64
	At           time.Time
	ValueUSDC    string
	Targets      string // JSON
	Actions      string // JSON
	Note         string
}

// RecordTick stores a strategy tick.
func (s *Store) RecordTick(ctx context.Context, t Tick) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO strategy_ticks(activation_id, at, value_usdc, targets, actions, note)
		VALUES(?,?,?,?,?,?)`, t.ActivationID, ts(t.At), t.ValueUSDC, t.Targets, t.Actions, t.Note)
	return err
}

// TickRange returns the first and last tick of an activation and the count.
func (s *Store) TickRange(ctx context.Context, activationID int64) (first, last Tick, n int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM strategy_ticks WHERE activation_id=?`, activationID).Scan(&n)
	if err != nil || n == 0 {
		return first, last, n, err
	}
	get := func(order string) (Tick, error) {
		var t Tick
		var at string
		err := s.db.QueryRowContext(ctx, `SELECT activation_id, at, value_usdc, targets, actions, note FROM strategy_ticks
			WHERE activation_id=? ORDER BY id `+order+` LIMIT 1`, activationID).
			Scan(&t.ActivationID, &at, &t.ValueUSDC, &t.Targets, &t.Actions, &t.Note)
		t.At = parseTS(at)
		return t, err
	}
	if first, err = get("ASC"); err != nil {
		return
	}
	last, err = get("DESC")
	return
}

// RecentTicks returns the last n ticks of an activation, newest first.
func (s *Store) RecentTicks(ctx context.Context, activationID int64, n int) ([]Tick, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT activation_id, at, value_usdc, targets, actions, note FROM strategy_ticks
		WHERE activation_id=? ORDER BY id DESC LIMIT ?`, activationID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tick
	for rows.Next() {
		var t Tick
		var at string
		if err := rows.Scan(&t.ActivationID, &at, &t.ValueUSDC, &t.Targets, &t.Actions, &t.Note); err != nil {
			return nil, err
		}
		t.At = parseTS(at)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- agent wake-ups ----------------------------------------------------------

// Wakeup is a market event that should prompt an early agent review.
type Wakeup struct {
	ID     int64
	At     time.Time
	Kind   string
	Key    string
	Detail string
	RunID  string // empty while pending
}

// AddWakeup records a wake-up unless one with the same key was recorded
// within cooldown; added reports whether it was recorded.
func (s *Store) AddWakeup(ctx context.Context, w Wakeup, cooldown time.Duration) (added bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM wakeups WHERE key=? AND at > ?`,
			w.Key, ts(w.At.Add(-cooldown))).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO wakeups(at, kind, key, detail) VALUES(?,?,?,?)`, ts(w.At), w.Kind, w.Key, w.Detail)
		added = err == nil
		return err
	})
	return added, err
}

// ClaimWakeups marks all pending wake-ups as handled by runID and returns
// them, oldest first.
func (s *Store) ClaimWakeups(ctx context.Context, runID string, at time.Time) ([]Wakeup, error) {
	var out []Wakeup
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, at, kind, key, detail FROM wakeups WHERE run_id IS NULL ORDER BY at, id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var w Wakeup
			var wat string
			if err := rows.Scan(&w.ID, &wat, &w.Kind, &w.Key, &w.Detail); err != nil {
				rows.Close()
				return err
			}
			w.At, w.RunID = parseTS(wat), runID
			out = append(out, w)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(out) == 0 {
			return err
		}
		if err := ensureRun(ctx, tx, runID, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE wakeups SET run_id=? WHERE run_id IS NULL`, runID)
		return err
	})
	return out, err
}

// PendingWakeups counts wake-ups no run has handled yet.
func (s *Store) PendingWakeups(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM wakeups WHERE run_id IS NULL`).Scan(&n)
	return n, err
}

// LastRunStart returns when the most recent agent run started.
func (s *Store) LastRunStart(ctx context.Context) (time.Time, bool, error) {
	var at sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT max(started_at) FROM runs`).Scan(&at); err != nil || !at.Valid {
		return time.Time{}, false, err
	}
	return parseTS(at.String), true, nil
}

// ---- helpers ---------------------------------------------------------------

func toInt64(n *big.Int) (int64, error) {
	if n == nil {
		return 0, errors.New("nil amount")
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("amount %s overflows int64", n)
	}
	return n.Int64(), nil
}

func nullInt(n *big.Int) any {
	if n == nil || !n.IsInt64() {
		return nil
	}
	return n.Int64()
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
