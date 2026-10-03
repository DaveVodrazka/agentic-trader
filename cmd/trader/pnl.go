package main

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"agentic-trader/internal/pnl"
	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

// cmdPnL values the wallet at live prices, saves the result as a manual
// snapshot and prints it.
func cmdPnL(ctx context.Context, app *app) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, snap, err := pnl.Take(ctx, app.store, app.jupiter(), app.tokens, store.KindManual, "")
	if err != nil {
		return err
	}
	printReport(r, app.tokens)

	// Compare with the previous snapshot on the PnL timeline.
	prevID, _, err := app.store.AdjacentSnapshots(ctx, snap.ID, store.KindManual, store.KindInitial)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("Saved as snapshot #%d", snap.ID)
	if prev, ok, err := app.store.GetSnapshot(ctx, prevID); err == nil && ok {
		pv, _ := new(big.Rat).SetString(prev.ValueUSDC)
		delta := new(big.Rat).Sub(r.Total, pv)
		line += fmt.Sprintf(" · %s since #%d (%s, %s ago)", color(signed(delta), delta.Sign()), prev.ID, prev.Kind,
			humanDuration(snap.TakenAt.Sub(prev.TakenAt)))
	}
	fmt.Printf("  %s\n\n", line)
	return nil
}

func printReport(r *pnl.Report, tokens *venue.TokenRegistry) {
	fmt.Println()
	fmt.Println("  PORTFOLIO PnL")
	fmt.Println("  " + strings.Repeat("─", 52))
	row("Started", r.StartedAt.Local().Format("2006-01-02 15:04 MST"))
	row("Valued", r.At.Local().Format("2006-01-02 15:04 MST"))
	row("Running for", humanDuration(r.Elapsed))
	fmt.Println()
	row("Initial value", usd(r.InitialValue))
	current := usd(r.Total)
	if r.Unvalued > 0 {
		current += fmt.Sprintf("  \033[33m⚠ incomplete: %d holding(s) unpriced\033[0m", r.Unvalued)
	}
	row("Current value", current)
	if r.Unvalued > 0 {
		row("", "\033[33m⚠ PnL below is wrong until all holdings are priced; retry shortly\033[0m")
	}
	row("PnL", color(signed(r.PnL)+fmt.Sprintf("  (%+.2f%%)", r.ReturnPct), r.PnL.Sign()))
	row("APR (simple)", color(pct(r.APR), sign(r.APR)))
	row("APY (compounded)", color(pct(r.APY), sign(r.APY)))
	if r.Elapsed < 7*24*time.Hour {
		row("", "\033[2m⚠ annualised from "+humanDuration(r.Elapsed)+"; not meaningful yet\033[0m")
	}

	printCosts(r)

	fmt.Println()
	fmt.Println("  HOLDINGS (liquidation value in USDC)")
	fmt.Println("  " + strings.Repeat("─", 52))
	for _, h := range r.Holdings {
		val := "unpriced: " + errShort(h.Err)
		if h.Value != nil {
			val = fmt.Sprintf("%12s  %5.1f%%", usd(h.Value), h.Weight*100)
		}
		fmt.Printf("  %-6s %22s  %s\n", h.Symbol, h.Amount, val)
	}
	fmt.Printf("  %-6s %22s  %s\n", "start", "", depositSummary(r.Deposits, tokens))
	if r.Unvalued > 0 {
		fmt.Printf("  \033[33m⚠ %d holding(s) could not be priced; current value is understated\033[0m\n", r.Unvalued)
	}

	fmt.Println()
	fmt.Println("  ACTIVITY")
	fmt.Println("  " + strings.Repeat("─", 52))
	row("Agent runs", fmt.Sprint(r.Runs))
	row("Trades", fmt.Sprint(r.Trades.Count))
	if r.Trades.Count > 0 {
		row("Volume (USDC legs)", usd(r.Trades.VolumeUSDC))
		if r.InitialValue.Sign() > 0 {
			turnover, _ := new(big.Rat).Quo(r.Trades.VolumeUSDC, r.InitialValue).Float64()
			row("Turnover", fmt.Sprintf("%.2fx initial capital", turnover))
		}
		row("First trade", r.Trades.First.Local().Format("2006-01-02 15:04"))
		row("Last trade", r.Trades.Last.Local().Format("2006-01-02 15:04")+"  ("+humanDuration(r.At.Sub(r.Trades.Last))+" ago)")
		row("Pairs", pairs(r.Trades.ByPair))
	}
	row("Cash (USDC)", fmt.Sprintf("%.1f%% of portfolio", r.CashPct*100))
	fmt.Println()
	fmt.Println("  \033[2mValues are what each holding would sell for into USDC now via Jupiter")
	fmt.Println("  (after price impact, before network fees).\033[0m")
	fmt.Println()
}

func printCosts(r *pnl.Report) {
	c := r.Costs
	fmt.Println()
	fmt.Println("  COSTS")
	fmt.Println("  " + strings.Repeat("─", 52))
	if c.Tracked == 0 {
		row("", "\033[2mno trades with fee data yet\033[0m")
	} else {
		row("Network fees", fmt.Sprintf("%s SOL  %s", c.NetworkSOL.FloatString(6), usd4(c.NetworkUSDC)))
		row("Price impact (est.)", usd4(c.ImpactUSDC))
		row("Platform fees", usd4(c.PlatformUSDC))
		total := c.Total()
		share := ""
		if r.InitialValue.Sign() > 0 {
			f, _ := new(big.Rat).Quo(total, r.InitialValue).Float64()
			share = fmt.Sprintf("  (%.3f%% of capital)", f*100)
		}
		row("Total costs", usd4(total)+share)
		row("Gross PnL", color(signed(r.GrossPnL), r.GrossPnL.Sign())+"  before costs")
		row("Net PnL", color(signed(r.PnL), r.PnL.Sign())+eaten(r))
	}
	if c.Untracked > 0 {
		since := "fee tracking started"
		if !c.Since.IsZero() {
			since = "tracked since " + c.Since.Local().Format("2006-01-02 15:04")
		}
		row("", fmt.Sprintf("\033[2m%d earlier trade(s) have no fee data; %s\033[0m", c.Untracked, since))
	}
	if c.Unpriced > 0 {
		row("", fmt.Sprintf("\033[33m⚠ %d trade(s) had costs that couldn't be valued in USDC\033[0m", c.Unpriced))
	}
}

// eaten describes how much of the gross profit went to costs.
func eaten(r *pnl.Report) string {
	if r.GrossPnL.Sign() <= 0 {
		return ""
	}
	f, _ := new(big.Rat).Quo(r.Costs.Total(), r.GrossPnL).Float64()
	return fmt.Sprintf("  \033[2mcosts ate %.0f%% of gross\033[0m", f*100)
}

func usd4(r *big.Rat) string {
	if r.Sign() != 0 && new(big.Rat).Abs(r).Cmp(big.NewRat(1, 100)) < 0 {
		return "$" + r.FloatString(4)
	}
	return usd(r)
}

func row(label, value string) { fmt.Printf("  %-20s %s\n", label, value) }

func usd(r *big.Rat) string {
	s := r.FloatString(2)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if neg {
		return "-$" + whole + "." + frac
	}
	return "$" + whole + "." + frac
}

func signed(r *big.Rat) string {
	if r.Sign() > 0 {
		return "+" + usd(r)
	}
	return usd(r)
}

func pct(f float64) string {
	switch {
	case math.IsInf(f, 1) || f > 1e6:
		return "> +1,000,000%"
	case math.IsNaN(f):
		return "n/a"
	}
	return fmt.Sprintf("%+.2f%%", f)
}

func sign(f float64) int {
	switch {
	case f > 0:
		return 1
	case f < 0:
		return -1
	}
	return 0
}

func color(s string, sign int) string {
	switch sign {
	case 1:
		return "\033[32m" + s + "\033[0m"
	case -1:
		return "\033[31m" + s + "\033[0m"
	}
	return s
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func depositSummary(ds []store.Deposit, tokens *venue.TokenRegistry) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		amt := d.Amount.String()
		if tok, err := tokens.Lookup(d.Symbol); err == nil {
			amt = venue.FormatUnits(d.Amount, tok.Decimals)
		}
		parts = append(parts, amt+" "+d.Symbol)
	}
	return "\033[2mdeposited " + strings.Join(parts, ", ") + "\033[0m"
}

func pairs(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s ×%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func errShort(err error) string {
	if err == nil {
		return "unknown"
	}
	s := err.Error()
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return s
}
