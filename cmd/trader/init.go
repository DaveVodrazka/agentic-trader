package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentic-trader/internal/legacy"
	"agentic-trader/internal/pnl"
	"agentic-trader/internal/store"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

// runInit creates the database. If legacy state files exist next to it, they
// are imported (into a temp database first, so a failed import leaves
// nothing behind) and then moved into legacy/. Otherwise the wallet is
// funded with fresh deposits.
func runInit(ctx context.Context, app *app, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	usdc := fs.String("usdc", "1000", "USDC to deposit (fresh start only)")
	sol := fs.String("sol", "0.05", "SOL to deposit for network fees (fresh start only)")
	fs.Parse(args)

	if ok, err := app.store.Initialized(ctx); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s is already initialized", app.dbPath)
	}
	if err := app.syncTokens(ctx); err != nil {
		return err
	}

	dir := filepath.Dir(app.dbPath)
	if legacy.Present(dir) {
		return importLegacy(ctx, app, dir)
	}
	return freshStart(ctx, app, *usdc, *sol)
}

func freshStart(ctx context.Context, app *app, usdcAmt, solAmt string) error {
	now := time.Now()
	type dep struct{ sym, amt string }
	for _, d := range []dep{{"USDC", usdcAmt}, {"SOL", solAmt}} {
		tok, err := app.tokens.Lookup(d.sym)
		if err != nil {
			return err
		}
		amt, err := venue.ParseUnits(d.amt, tok.Decimals)
		if errors.Is(err, venue.ErrInvalidAmount) && (d.amt == "0" || d.amt == "") {
			continue
		}
		if err != nil {
			return fmt.Errorf("-%s: %w", d.sym, err)
		}
		// The deposit value is the PnL baseline, so it is fixed now.
		value, err := trading.VenuePricer{Venue: app.jupiter()}.ValueUSDC(ctx, tok, amt)
		if err != nil {
			return fmt.Errorf("price %s deposit: %w", d.sym, err)
		}
		if err := app.store.AddDeposit(ctx, store.Deposit{At: now, Symbol: tok.Symbol, Amount: amt,
			ValueUSDC: value.FloatString(6), Note: "initial deposit"}); err != nil {
			return err
		}
		fmt.Printf("deposited %s %s (%s USDC)\n", d.amt, tok.Symbol, value.FloatString(2))
	}
	if err := recordInitialSnapshot(ctx, app.store, app.tokens); err != nil {
		return err
	}
	fmt.Printf("initialized %s\n", app.dbPath)
	return nil
}

// recordInitialSnapshot stores the starting wallet as snapshot kind
// "initial", the first point on the PnL timeline.
func recordInitialSnapshot(ctx context.Context, st *store.Store, tokens *venue.TokenRegistry) error {
	deposits, err := st.Deposits(ctx)
	if err != nil {
		return err
	}
	return st.RecordSnapshot(ctx, pnl.InitialSnapshot(deposits, tokens))
}

func importLegacy(ctx context.Context, a *app, dir string) error {
	tmpPath := a.dbPath + ".import"
	removeDB(tmpPath)
	tmp, err := store.Open(tmpPath)
	if err != nil {
		return err
	}
	tmpApp := &app{dbPath: tmpPath, store: tmp, tokens: a.tokens}
	if err := tmpApp.syncTokens(ctx); err != nil {
		tmp.Close()
		return err
	}
	sum, err := legacy.Import(ctx, tmp, a.tokens, dir)
	if err == nil {
		err = recordInitialSnapshot(ctx, tmp, a.tokens)
	}
	tmp.Close()
	if err != nil {
		removeDB(tmpPath)
		return fmt.Errorf("import legacy files: %w", err)
	}

	// Swap the imported database in place of the empty one.
	a.store.Close()
	removeDB(a.dbPath)
	if err := os.Rename(tmpPath, a.dbPath); err != nil {
		return err
	}
	if a.store, err = store.Open(a.dbPath); err != nil {
		return err
	}
	if err := a.memory().WriteView(ctx); err != nil {
		return err
	}

	backup := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		return err
	}
	for _, f := range legacy.Files {
		if f == "NARRATIVE.md" {
			continue // now generated from the database
		}
		if err := os.Rename(filepath.Join(dir, f), filepath.Join(backup, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	fmt.Printf("imported %d deposit(s), %d trade(s), %d journal entr(ies), %d narrative(s), %d run(s) into %s\n",
		sum.Deposits, sum.Trades, sum.Journal, sum.Narratives, sum.Runs, a.dbPath)
	fmt.Printf("old files moved to %s/\n", backup)
	return nil
}

// removeDB deletes a database and its WAL side files.
func removeDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
}
