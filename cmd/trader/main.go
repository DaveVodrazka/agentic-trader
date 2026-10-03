// Command trader is the agentic trader's CLI: the MCP server the agent
// trades through, plus setup, run bookkeeping and reports.
//
// All state lives in a SQLite database (default trader.db, or $TRADER_DB).
// Stdout is reserved for command output (and the MCP protocol); logs go to
// stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"agentic-trader/internal/memory"
	"agentic-trader/internal/store"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, app *app, args []string) error
}

var commands = []command{
	{"mcp", "serve the trading tools over MCP on stdio", runMCP},
	{"init", "create the database (imports legacy files if present)", runInit},
	{"begin-run", "record a run's start and print the agent's memory", runBeginRun},
	{"end-run", "record a run's result and snapshot the portfolio", runEndRun},
	{"tick", "record prices and run the live strategy once", runTick},
	{"strategy", "list | show | history | set <name> -reason ...", runStrategy},
	{"backtest", "backtest <name> [-params JSON] [-days N] vs benchmarks", runBacktest},
	{"backfill", "load historical price bars (GeckoTerminal)", runBackfill},
	{"pnl", "print the profit & loss report", noArgs(cmdPnL)},
	{"narrative", "print the latest narrative", noArgs(cmdNarrative)},
	{"journal", "print the journal", noArgs(cmdJournal)},
	{"trades", "print trades with reasons and fees", noArgs(cmdTrades)},
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("trader: ")

	dbPath := flag.String("db", envOr("TRADER_DB", store.DefaultPath), "SQLite database path")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	name, args := flag.Arg(0), flag.Args()[1:]
	var cmd *command
	for i := range commands {
		if commands[i].name == name {
			cmd = &commands[i]
		}
	}
	if cmd == nil {
		log.Printf("unknown command %q", name)
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := newApp(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer app.close()
	if err := cmd.run(ctx, app, args); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: trader [-db path] <command> [flags]\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.name, c.summary)
	}
}

func noArgs(fn func(context.Context, *app) error) func(context.Context, *app, []string) error {
	return func(ctx context.Context, a *app, args []string) error {
		if len(args) > 0 {
			return fmt.Errorf("unexpected arguments: %v", args)
		}
		return fn(ctx, a)
	}
}

// app holds what every command needs.
type app struct {
	dbPath string
	store  *store.Store
	tokens *venue.TokenRegistry
}

func newApp(dbPath string) (*app, error) {
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &app{dbPath: dbPath, store: st, tokens: venue.NewTokenRegistry(venue.SolanaTokens...)}, nil
}

func (a *app) close() { a.store.Close() }

func (a *app) jupiter() *venue.Jupiter {
	return venue.NewJupiter(venue.WithJupiterTokens(a.tokens), venue.WithJupiterAPIKey(os.Getenv("JUPITER_API_KEY")))
}

// memory returns the narrative store; NARRATIVE.md sits next to the database.
func (a *app) memory() *memory.Memory {
	return memory.New(a.store, filepath.Join(filepath.Dir(a.dbPath), memory.DefaultNarrativePath), 0)
}

func (a *app) ledger() *trading.Ledger { return trading.NewLedger(a.store, a.tokens) }

// syncTokens records token metadata so the database is self-describing
// (decimals for formatting amounts, mints).
func (a *app) syncTokens(ctx context.Context) error {
	toks := make([]store.Token, 0, len(venue.SolanaTokens))
	for _, t := range venue.SolanaTokens {
		toks = append(toks, store.Token{Symbol: t.Symbol, Mint: t.Address, Decimals: t.Decimals})
	}
	return a.store.UpsertTokens(ctx, toks)
}

func (a *app) requireInitialized(ctx context.Context) error {
	ok, err := a.store.Initialized(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return store.ErrNotInitialized
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
