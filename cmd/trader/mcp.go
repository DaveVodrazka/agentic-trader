package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentic-trader/internal/mcpserver"
	"agentic-trader/internal/trading"
)

// runMCP serves the trading tools on stdio for one agent run. Concurrent
// servers are safe: balance checks and updates are database transactions.
func runMCP(ctx context.Context, app *app, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	fs.Parse(args)

	if err := app.requireInitialized(ctx); err != nil {
		return err
	}
	if err := app.syncTokens(ctx); err != nil {
		return err
	}
	runID := os.Getenv("TRADER_RUN_ID")
	if runID == "" {
		runID = "run-" + time.Now().UTC().Format("20060102T150405Z")
	}
	log.Printf("run %s, db %s", runID, app.dbPath)

	jup := app.jupiter()
	sol, err := app.tokens.Lookup("SOL")
	if err != nil {
		return err
	}
	costs := trading.CostEstimator{
		Fees:   trading.DefaultPaperFees,
		Pricer: &trading.CachedPricer{Pricer: trading.VenuePricer{Venue: jup}, TTL: 30 * time.Second},
		SOL:    sol,
	}
	limits := trading.DefaultLimits
	ledger := app.ledger()

	srv := mcpserver.New(mcpserver.Config{
		Venue:    jup,
		Executor: trading.NewPaperExecutor(ledger, limits, costs),
		Ledger:   ledger,
		Memory:   app.memory(),
		Store:    app.store,
		Costs:    costs,
		QuoteTTL: limits.MaxQuoteAge,
		RunID:    runID,
	}).MCP()
	return srv.Run(ctx, &mcp.StdioTransport{})
}
