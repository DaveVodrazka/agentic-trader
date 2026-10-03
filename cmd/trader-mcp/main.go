// Command trader-mcp serves trading tools over MCP on stdio.
//
// Stdout carries the MCP protocol; all logging goes to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentic-trader/internal/mcpserver"
	"agentic-trader/internal/memory"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("trader-mcp: ")
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	walletPath := flag.String("wallet", trading.DefaultWalletPath, "wallet snapshot file")
	tradesPath := flag.String("trades", trading.DefaultTradesPath, "append-only trade log")
	narrativePath := flag.String("narrative", memory.DefaultNarrativePath, "agent narrative (working memory)")
	journalPath := flag.String("journal", memory.DefaultJournalPath, "append-only agent journal")
	flag.Parse()

	runID := os.Getenv("TRADER_RUN_ID")
	if runID == "" {
		runID = "run-" + time.Now().UTC().Format("20060102T150405Z")
	}
	log.Printf("run %s", runID)

	// One trader per wallet: a second process would overwrite our snapshot.
	unlock, err := lockFile(*walletPath + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	ledger, err := trading.OpenLedger(*walletPath, *tradesPath, tokens)
	if err != nil {
		return err
	}
	jup := venue.NewJupiter(
		venue.WithJupiterTokens(tokens),
		venue.WithJupiterAPIKey(os.Getenv("JUPITER_API_KEY")),
	)
	sol, err := tokens.Lookup("SOL")
	if err != nil {
		return err
	}
	costs := trading.CostEstimator{Fees: trading.DefaultPaperFees, Pricer: &trading.CachedPricer{Pricer: trading.VenuePricer{Venue: jup}, TTL: 30 * time.Second}, SOL: sol}
	limits := trading.DefaultLimits
	ex := trading.NewPaperExecutor(ledger, limits, costs)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := mcpserver.New(mcpserver.Config{
		Venue:    jup,
		Executor: ex,
		Ledger:   ledger,
		Memory:   memory.NewStore(*narrativePath, *journalPath, 0),
		Costs:    costs,
		QuoteTTL: limits.MaxQuoteAge,
		RunID:    runID,
	}).MCP()
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// lockFile takes an exclusive, non-blocking advisory lock on path.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("wallet %s is in use by another trader process: %w", path, err)
	}
	return func() { f.Close() }, nil
}
