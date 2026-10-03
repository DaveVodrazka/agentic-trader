package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"agentic-trader/internal/pnl"
	"agentic-trader/internal/store"
)

// runBeginRun records the start of an agent run, claims pending wake-ups,
// and prints the context block (why it runs, narrative, journal) for the
// agent's system prompt.
func runBeginRun(ctx context.Context, app *app, args []string) error {
	fs := flag.NewFlagSet("begin-run", flag.ExitOnError)
	runID := fs.String("run-id", "", "run ID (required)")
	prompt := fs.String("prompt", "", "the prompt given to the agent")
	fs.Parse(args)
	if *runID == "" {
		return errors.New("-run-id is required")
	}
	if err := app.requireInitialized(ctx); err != nil {
		return err
	}
	now := time.Now()
	if err := app.store.BeginRun(ctx, *runID, *prompt, now); err != nil {
		return err
	}
	wakeups, err := app.store.ClaimWakeups(ctx, *runID, now)
	if err != nil {
		return err
	}
	block, err := app.memory().Context(ctx, *runID, wakeups)
	if err != nil {
		return err
	}
	fmt.Print(block)
	return nil
}

// runEndRun records a run's outcome (exit code, the agent's output) and
// snapshots the portfolio value for the equity curve. A failed snapshot is
// logged but does not fail the command: the run record matters more.
func runEndRun(ctx context.Context, app *app, args []string) error {
	fs := flag.NewFlagSet("end-run", flag.ExitOnError)
	runID := fs.String("run-id", "", "run ID (required)")
	exitCode := fs.Int("exit-code", 0, "the agent's exit code")
	logPath := fs.String("log", "", "log file holding the agent's output")
	fs.Parse(args)
	if *runID == "" {
		return errors.New("-run-id is required")
	}

	now := time.Now()
	r := store.Run{ID: *runID, StartedAt: now, FinishedAt: now, ExitCode: exitCode, LogPath: *logPath}
	if *logPath != "" {
		data, err := os.ReadFile(*logPath)
		if err != nil {
			log.Printf("read log: %v", err)
		}
		r.Report = string(data)
	}
	if err := app.store.EndRun(ctx, r); err != nil {
		return err
	}

	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rep, snap, err := pnl.Take(vctx, app.store, app.jupiter(), app.tokens, store.KindRun, *runID)
	if err != nil {
		log.Printf("snapshot: %v", err)
		return nil
	}
	status := "complete"
	if !snap.Complete {
		status = fmt.Sprintf("incomplete, %d unpriced", rep.Unvalued)
	}
	fmt.Printf("snapshot #%d: portfolio %s USDC, PnL %s (%s)\n", snap.ID, rep.Total.FloatString(2), rep.PnL.FloatString(2), status)
	return nil
}
