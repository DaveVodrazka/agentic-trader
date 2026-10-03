package main

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"agentic-trader/internal/venue"
)

func cmdNarrative(ctx context.Context, app *app) error {
	n, ok, err := app.store.LatestNarrative(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("no narrative yet")
		return nil
	}
	fmt.Printf("\033[2m%s · %s\033[0m\n\n%s\n", n.At.Local().Format("2006-01-02 15:04"), n.RunID, n.Body)
	return nil
}

func cmdJournal(ctx context.Context, app *app) error {
	entries, err := app.store.Journal(ctx, 0)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no journal yet")
	}
	for _, e := range entries {
		fmt.Printf("\033[2m%s  %s\033[0m\n  %s\n", e.At.Local().Format("2006-01-02 15:04"), e.RunID, e.Summary)
		if len(e.TradeIDs) > 0 {
			fmt.Printf("  trades: %s\n", strings.Join(e.TradeIDs, ", "))
		}
		fmt.Println()
	}
	return nil
}

func cmdTrades(ctx context.Context, app *app) error {
	trades, err := app.store.Trades(ctx)
	if err != nil {
		return err
	}
	if len(trades) == 0 {
		fmt.Println("no trades yet")
	}
	for _, t := range trades {
		from, _ := app.tokens.Lookup(t.From)
		to, _ := app.tokens.Lookup(t.To)
		fmt.Printf("\033[2m%s  #%d %s\033[0m\n  %s %s -> %s %s @ %s\n",
			t.At.Local().Format("2006-01-02 15:04"), t.Seq, t.ID,
			venue.FormatUnits(t.InAmount, from.Decimals), t.From, venue.FormatUnits(t.OutAmount, to.Decimals), t.To, t.Price)
		if f := t.Fees; f != nil {
			fmt.Printf("  fees: %s SOL network", venue.FormatUnits(bigInt(f.NetworkLamports), 9))
			if f.NetworkUSDC != "" || f.ImpactUSDC != "" {
				fmt.Printf(" ($%s), $%s price impact", f.NetworkUSDC, f.ImpactUSDC)
			}
			fmt.Println()
		}
		fmt.Printf("  why: %s\n\n", t.Reason)
	}
	return nil
}

func bigInt(n int64) *big.Int { return big.NewInt(n) }
