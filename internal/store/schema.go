package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; never edit one that has shipped, append a
// new one instead. The index+1 is the schema version.
var migrations = []string{
	// v1: initial schema.
	`
CREATE TABLE tokens (
	symbol   TEXT PRIMARY KEY,
	mint     TEXT NOT NULL,
	decimals INTEGER NOT NULL
) STRICT;

-- Capital added to the wallet; the PnL baseline. First row = start.
CREATE TABLE deposits (
	id         INTEGER PRIMARY KEY,
	at         TEXT NOT NULL,
	symbol     TEXT NOT NULL REFERENCES tokens(symbol),
	amount     INTEGER NOT NULL,
	value_usdc TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT ''
) STRICT;

-- Current holdings, maintained in the same transaction as each trade.
CREATE TABLE balances (
	symbol     TEXT PRIMARY KEY REFERENCES tokens(symbol),
	amount     INTEGER NOT NULL CHECK (amount >= 0),
	updated_at TEXT NOT NULL
) STRICT;

-- One agent activation.
CREATE TABLE runs (
	id          TEXT PRIMARY KEY,
	started_at  TEXT NOT NULL,
	finished_at TEXT,
	exit_code   INTEGER,
	prompt      TEXT,
	report      TEXT,
	log_path    TEXT
) STRICT;

CREATE TABLE trades (
	id                   TEXT PRIMARY KEY,
	seq                  INTEGER NOT NULL UNIQUE,
	run_id               TEXT REFERENCES runs(id),
	at                   TEXT NOT NULL,
	venue                TEXT NOT NULL,
	paper                INTEGER NOT NULL,
	from_symbol          TEXT NOT NULL REFERENCES tokens(symbol),
	to_symbol            TEXT NOT NULL REFERENCES tokens(symbol),
	in_amount            INTEGER NOT NULL,
	out_amount           INTEGER NOT NULL,
	quoted_out           INTEGER,
	price                TEXT NOT NULL,
	reason               TEXT NOT NULL,
	fees_tracked         INTEGER NOT NULL,
	network_fee_lamports INTEGER NOT NULL DEFAULT 0,
	platform_fee         INTEGER,
	price_impact_pct     TEXT,
	network_fee_usdc     TEXT,
	platform_fee_usdc    TEXT,
	impact_usdc          TEXT
) STRICT;
CREATE INDEX trades_at ON trades(at);
CREATE INDEX trades_run ON trades(run_id);

-- The agent's per-run summary.
CREATE TABLE journal (
	id      INTEGER PRIMARY KEY,
	run_id  TEXT NOT NULL REFERENCES runs(id),
	at      TEXT NOT NULL,
	summary TEXT NOT NULL
) STRICT;
CREATE INDEX journal_at ON journal(at);

-- Every version of the agent's working memory.
CREATE TABLE narratives (
	id     INTEGER PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES runs(id),
	at     TEXT NOT NULL,
	body   TEXT NOT NULL
) STRICT;
CREATE INDEX narratives_at ON narratives(at);

-- Every quote the agent saw; trade_id set if it was executed.
CREATE TABLE quotes (
	id               INTEGER PRIMARY KEY,
	quote_id         TEXT NOT NULL,
	run_id           TEXT REFERENCES runs(id),
	at               TEXT NOT NULL,
	venue            TEXT NOT NULL,
	from_symbol      TEXT NOT NULL REFERENCES tokens(symbol),
	to_symbol        TEXT NOT NULL REFERENCES tokens(symbol),
	in_amount        INTEGER NOT NULL,
	out_amount       INTEGER NOT NULL,
	min_out          INTEGER,
	price            TEXT NOT NULL,
	price_impact_pct TEXT NOT NULL,
	route            TEXT NOT NULL,
	trade_id         TEXT REFERENCES trades(id)
) STRICT;
CREATE INDEX quotes_at ON quotes(at);
CREATE INDEX quotes_quote_id ON quotes(quote_id);

-- Token prices in USDC over time (price charts).
CREATE TABLE prices (
	id         INTEGER PRIMARY KEY,
	at         TEXT NOT NULL,
	symbol     TEXT NOT NULL REFERENCES tokens(symbol),
	price_usdc TEXT NOT NULL,
	source     TEXT NOT NULL
) STRICT;
CREATE INDEX prices_symbol_at ON prices(symbol, at);

-- PnL snapshots: immutable, self-contained records of the portfolio at a
-- point in time, with every figure the PnL page shows. Navigate by id.
--   kind 'initial': the starting wallet (deposits), once
--   kind 'run':     after each agent run (equity curve)
--   kind 'manual':  on request (make pnl, website button)
-- Cumulative figures (costs, trade/run counts) are as of taken_at.
CREATE TABLE snapshots (
	id               INTEGER PRIMARY KEY,
	taken_at         TEXT NOT NULL,
	kind             TEXT NOT NULL CHECK (kind IN ('initial', 'run', 'manual')),
	run_id           TEXT REFERENCES runs(id),
	complete         INTEGER NOT NULL,  -- 0 if a holding could not be priced
	value_usdc       TEXT NOT NULL,
	deposits_usdc    TEXT NOT NULL,
	pnl_usdc         TEXT NOT NULL,
	return_pct       TEXT NOT NULL,
	apr_pct          TEXT,              -- NULL when not meaningful (initial, overflow)
	apy_pct          TEXT,
	gross_pnl_usdc   TEXT NOT NULL,     -- pnl before costs
	costs_usdc       TEXT NOT NULL,     -- network + price impact + platform
	network_fees_sol TEXT NOT NULL,
	cash_pct         TEXT NOT NULL,
	trade_count      INTEGER NOT NULL,
	run_count        INTEGER NOT NULL
) STRICT;
CREATE INDEX snapshots_kind_id ON snapshots(kind, id);
CREATE INDEX snapshots_taken_at ON snapshots(taken_at);

-- Per-token rows of a snapshot.
CREATE TABLE snapshot_holdings (
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
	symbol      TEXT NOT NULL REFERENCES tokens(symbol),
	amount      INTEGER NOT NULL,   -- smallest units
	price_usdc  TEXT,               -- NULL if unpriced; USDC is 1
	value_usdc  TEXT,
	weight_pct  TEXT,
	error       TEXT,               -- why pricing failed
	PRIMARY KEY (snapshot_id, symbol)
) STRICT;
`,

	// v2: strategies and market data.
	`
-- OHLC price bars in USD. interval: '5m', '1h', '1d'. start is the bar's
-- open time. Floats: these drive signals and charts, not accounting.
CREATE TABLE candles (
	symbol   TEXT NOT NULL REFERENCES tokens(symbol),
	interval TEXT NOT NULL,
	start    TEXT NOT NULL,
	open     REAL NOT NULL,
	high     REAL NOT NULL,
	low      REAL NOT NULL,
	close    REAL NOT NULL,
	source   TEXT NOT NULL,
	PRIMARY KEY (symbol, interval, start)
) STRICT;

-- Which strategy runs, with what parameters and why. At most one row has
-- ended_at NULL (the live one). state holds the strategy's and risk
-- layer's memory between ticks (JSON).
CREATE TABLE strategy_activations (
	id          INTEGER PRIMARY KEY,
	strategy    TEXT NOT NULL,
	params      TEXT NOT NULL,
	reason      TEXT NOT NULL,
	set_by      TEXT NOT NULL,
	run_id      TEXT REFERENCES runs(id),
	started_at  TEXT NOT NULL,
	ended_at    TEXT,
	status      TEXT NOT NULL CHECK (status IN ('active', 'halted', 'ended')),
	halt_reason TEXT,
	state       TEXT NOT NULL DEFAULT '{}'
) STRICT;
CREATE UNIQUE INDEX strategy_activations_one_live ON strategy_activations((ended_at IS NULL)) WHERE ended_at IS NULL;

-- One row per strategy tick: what it wanted and what it did.
CREATE TABLE strategy_ticks (
	id            INTEGER PRIMARY KEY,
	activation_id INTEGER NOT NULL REFERENCES strategy_activations(id),
	at            TEXT NOT NULL,
	value_usdc    TEXT NOT NULL,
	targets       TEXT NOT NULL,  -- JSON {symbol: weight}
	actions       TEXT NOT NULL,  -- JSON [{from,to,amount,trade_id,error}]
	note          TEXT NOT NULL
) STRICT;
CREATE INDEX strategy_ticks_activation_at ON strategy_ticks(activation_id, at);

ALTER TABLE trades ADD COLUMN activation_id INTEGER REFERENCES strategy_activations(id);
`,

	// v3: incremental backfill bookkeeping.
	`
-- Per token and interval: which pool the provider prices it from, the
-- oldest bar the provider has served, and the newest bar already checked.
-- Missing bars before checked_through are provider gaps, not re-requested.
CREATE TABLE backfill_state (
	symbol          TEXT NOT NULL REFERENCES tokens(symbol),
	interval        TEXT NOT NULL,
	provider        TEXT NOT NULL,
	pool            TEXT NOT NULL,
	pool_name       TEXT NOT NULL,
	earliest        TEXT,
	checked_through TEXT,
	updated_at      TEXT NOT NULL,
	PRIMARY KEY (symbol, interval)
) STRICT;
`,

	// v4: event wake-ups for the agent.
	`
-- Market events that should wake the agent before its next scheduled
-- review. run_id is set when a run picks the wake-up up.
CREATE TABLE wakeups (
	id      INTEGER PRIMARY KEY,
	at      TEXT NOT NULL,
	kind    TEXT NOT NULL,  -- halt, stop, drawdown, move, stale
	key     TEXT NOT NULL,  -- debounce key, e.g. "move:SOL"
	detail  TEXT NOT NULL,
	run_id  TEXT REFERENCES runs(id)
) STRICT;
CREATE INDEX wakeups_key_at ON wakeups(key, at);
CREATE INDEX wakeups_pending ON wakeups(run_id) WHERE run_id IS NULL;
`,
}

// migrate applies pending migrations. The version is read inside the write
// transaction (BEGIN IMMEDIATE via the DSN), so processes opening the
// database at the same time cannot both apply the same migration.
func (s *Store) migrate(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if version > len(migrations) {
			return fmt.Errorf("database schema v%d is newer than this binary (v%d)", version, len(migrations))
		}
		for i := version; i < len(migrations); i++ {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("migrate to v%d: %w", i+1, err)
			}
		}
		if version == len(migrations) {
			return nil
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations)))
		return err
	})
}
