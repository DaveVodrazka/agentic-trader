package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agentic-trader/internal/mcpserver"
)

// runMCP serves the portfolio-manager tools on stdio for one agent run.
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

	srv := mcpserver.New(mcpserver.Config{
		Store:   app.store,
		Tokens:  app.tokens,
		Ledger:  app.ledger(),
		Memory:  app.memory(),
		Symbols: app.symbols(),
		RunID:   runID,
	}).MCP()
	return srv.Run(ctx, &mcp.StdioTransport{})
}
