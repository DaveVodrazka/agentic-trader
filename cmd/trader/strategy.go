package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"agentic-trader/internal/marketdata"
	"agentic-trader/internal/strategy"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

// stablecoins are not traded by strategies: USDC is the cash asset and other
// stablecoins only add fees.
var stablecoins = map[string]bool{"USDC": true, "USDT": true}

// universe is every token strategies may trade.
func (a *app) universe() []venue.Token {
	var out []venue.Token
	for _, t := range venue.SolanaTokens {
		if !stablecoins[t.Symbol] {
			out = append(out, t)
		}
	}
	return out
}

func (a *app) runner() *strategy.Runner {
	jup := a.jupiter()
	sol, _ := a.tokens.Lookup("SOL")
	costs := trading.CostEstimator{
		Fees:   trading.DefaultPaperFees,
		Pricer: &trading.CachedPricer{Pricer: trading.VenuePricer{Venue: jup}, TTL: 30 * time.Second},
		SOL:    sol,
	}
	return &strategy.Runner{
		Store: a.store, Tokens: a.tokens, Universe: a.universe(), Prices: jup, Venue: jup,
		Executor: trading.NewPaperExecutor(a.ledger(), trading.DefaultLimits, costs),
		Rules:    strategy.DefaultRules, Now: time.Now,
	}
}

// runTick records prices and runs the live strategy once.
func runTick(ctx context.Context, app *app, args []string) error {
	fs := flag.NewFlagSet("tick", flag.ExitOnError)
	quiet := fs.Bool("q", false, "print only trades, wake-ups and errors")
	wakeLabel := fs.String("wake", "", "launchd label of the agent job to kickstart on market events (empty: only record them)")
	rules := strategy.DefaultWakeRules
	fs.Float64Var(&rules.MoveHour, "wake-move", rules.MoveHour, "wake on a token moving this fraction within an hour")
	fs.Float64Var(&rules.DrawdownSinceReview, "wake-drawdown", rules.DrawdownSinceReview, "wake on the portfolio falling this fraction since the last review")
	fs.Parse(args)
	if err := app.requireInitialized(ctx); err != nil {
		return err
	}
	if err := app.syncTokens(ctx); err != nil {
		return err
	}
	res, tickErr := app.runner().Tick(ctx)

	// Market events may wake the agent, even if the tick itself failed.
	var waker strategy.Waker
	if *wakeLabel != "" {
		waker = launchdWaker(*wakeLabel)
	}
	watch, err := strategy.CheckWakeups(ctx, app.store, res, app.symbols(), rules, waker, time.Now())
	if err != nil {
		log.Printf("wake-ups: %v", err)
	}
	for _, w := range watch.Recorded {
		fmt.Printf("%s WAKE %s: %s\n", time.Now().Format("2006-01-02 15:04:05"), w.Kind, w.Detail)
	}
	if watch.Woke {
		fmt.Printf("  agent started (%d pending event(s))\n", watch.Pending)
	} else if len(watch.Recorded) > 0 && watch.Held != "" {
		fmt.Printf("  agent not started: %s\n", watch.Held)
	}

	if tickErr != nil {
		return tickErr
	}
	stamp := res.At.Local().Format("2006-01-02 15:04:05")
	if res.Activation == nil || res.Skipped != "" {
		if !*quiet {
			fmt.Printf("%s prices recorded (%d tokens); %s\n", stamp, len(res.Prices), res.Skipped)
		}
		return nil
	}
	a := res.Activation
	if !*quiet || len(res.Actions) > 0 || res.Plan.Halt != "" {
		fmt.Printf("%s %s #%d  portfolio $%.2f\n  %s\n", stamp, a.Strategy, a.ID, res.Portfolio.Total, res.Plan.Note())
	}
	for _, act := range res.Actions {
		if act.Error != "" {
			fmt.Printf("  ✗ %s %s -> %s ($%.2f): %s\n", act.Amount, act.From, act.To, act.ValueUSD, act.Error)
		} else {
			fmt.Printf("  ✓ %s %s -> %s ($%.2f) %s\n", act.Amount, act.From, act.To, act.ValueUSD, act.TradeID)
		}
	}
	if res.Plan.Halt != "" {
		fmt.Printf("  HALTED: %s\n  set a new strategy to resume\n", res.Plan.Halt)
	}
	return nil
}

// launchdWaker starts a launchd job now. launchd never runs two instances of
// a job, so waking a review already in progress is a no-op.
type launchdWaker string

func (l launchdWaker) Wake(ctx context.Context) error {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), string(l))
	if out, err := exec.CommandContext(ctx, "launchctl", "kickstart", target).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart %s: %v: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runBackfill brings historical candles up to date from GeckoTerminal.
func runBackfill(ctx context.Context, app *app, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	full := fs.Bool("full", false, "refetch the newest ~41 days hourly / ~6 months daily and replace stored bars")
	older := fs.Bool("older", false, "extend history one page (1000 hourly / 365 daily bars) further back")
	fs.Parse(args)
	if *full && *older {
		return errors.New("use -full or -older, not both")
	}
	if err := app.syncTokens(ctx); err != nil {
		return err
	}
	mode, label := marketdata.Incremental, "missing bars"
	switch {
	case *full:
		mode, label = marketdata.Full, "full refetch"
	case *older:
		mode, label = marketdata.Older, "older history"
	}
	g := marketdata.NewGeckoTerminal()
	g.OnRateLimit = func(wait time.Duration) { fmt.Printf("  … rate limited, waiting %s\n", wait) }
	fmt.Printf("backfill (%s) from GeckoTerminal:\n", label)
	failed, requests := 0, 0
	err := marketdata.Backfill(ctx, app.store, g, app.universe(), mode, time.Now(), func(r marketdata.BackfillResult) {
		requests += r.Requests
		if r.Err != nil {
			failed++
			fmt.Printf("  ✗ %-5s %v\n", r.Symbol, r.Err)
			return
		}
		fmt.Printf("  ✓ %-5s 1h: %-28s 1d: %s\n", r.Symbol, r.Status["1h"], r.Status["1d"])
	})
	if err != nil {
		return err
	}
	fmt.Printf("%d request(s)\n", requests)
	if failed > 0 {
		return fmt.Errorf("%d token(s) failed; rerun to retry", failed)
	}
	return nil
}

// runStrategy manages the live strategy.
func runStrategy(ctx context.Context, app *app, args []string) error {
	sub := "show"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list":
		for _, s := range strategy.Specs() {
			fmt.Printf("\033[1m%s\033[0m\n  %s\n  defaults: %s\n\n", s.Name, s.Summary, s.DefaultParams())
		}
		return nil
	case "show":
		return showStrategy(ctx, app)
	case "history":
		acts, err := app.store.Activations(ctx, 20)
		if err != nil {
			return err
		}
		for _, a := range acts {
			end := "live"
			if !a.EndedAt.IsZero() {
				end = a.EndedAt.Local().Format("01-02 15:04")
			}
			fmt.Printf("#%-3d %-10s %-7s %s → %-11s by %-5s %s\n     %s\n", a.ID, a.Strategy, a.Status,
				a.StartedAt.Local().Format("01-02 15:04"), end, a.SetBy, a.Params, a.Reason)
		}
		return nil
	case "set":
		if len(args) == 0 {
			return errors.New("usage: trader strategy set <name> [-params JSON] -reason TEXT [-force]")
		}
		name := args[0]
		fs := flag.NewFlagSet("strategy set", flag.ExitOnError)
		params := fs.String("params", "", "parameters as JSON (defaults if empty)")
		reason := fs.String("reason", "", "why (required)")
		force := fs.Bool("force", false, "replace the live strategy before its minimum run time")
		fs.Parse(args[1:])
		if err := app.requireInitialized(ctx); err != nil {
			return err
		}
		a, err := strategy.Activate(ctx, app.store, app.tokens, name, json.RawMessage(*params), *reason, "user", "", time.Now(), *force)
		if err != nil {
			return err
		}
		fmt.Printf("strategy %s #%d active with %s\nit trades on the next tick\n", a.Strategy, a.ID, a.Params)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q (list, show, history, set)", sub)
}

func showStrategy(ctx context.Context, app *app) error {
	a, ok, err := app.store.LiveStrategy(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("no strategy set; see 'trader strategy list'")
		return nil
	}
	fmt.Printf("\033[1m%s\033[0m #%d  %s since %s (%s ago), set by %s\n  params: %s\n  reason: %s\n",
		a.Strategy, a.ID, a.Status, a.StartedAt.Local().Format("2006-01-02 15:04"),
		humanDuration(time.Since(a.StartedAt)), a.SetBy, a.Params, a.Reason)
	if a.HaltReason != "" {
		fmt.Printf("  \033[31mhalted: %s\033[0m\n", a.HaltReason)
	}
	var st strategy.State
	if json.Unmarshal([]byte(a.State), &st) == nil {
		if len(st.Entry) > 0 {
			var parts []string
			for _, sym := range sortedKeys(st.Entry) {
				parts = append(parts, fmt.Sprintf("%s $%.4g", sym, st.Entry[sym]))
			}
			fmt.Printf("  entries: %s\n", strings.Join(parts, ", "))
		}
		if len(st.Stopped) > 0 {
			fmt.Printf("  stopped out: %s\n", strings.Join(st.Stopped, ", "))
		}
	}
	ticks, err := app.store.RecentTicks(ctx, a.ID, 5)
	if err != nil {
		return err
	}
	if len(ticks) > 0 {
		fmt.Println("  recent ticks:")
	}
	for _, t := range ticks {
		fmt.Printf("    %s  $%s  %s\n", t.At.Local().Format("01-02 15:04"), trimDecimals(t.ValueUSDC), t.Note)
	}
	return nil
}

// runBacktest replays a strategy over stored history, alongside benchmarks.
func runBacktest(ctx context.Context, app *app, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: trader backtest <name> [-params JSON] [-days 30]")
	}
	name := args[0]
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	params := fs.String("params", "", "parameters as JSON (defaults if empty)")
	days := fs.Int("days", 30, "days of history to test")
	fs.Parse(args[1:])

	end := time.Now().UTC().Truncate(time.Hour)
	cfg := strategy.DefaultBacktest(end, *days)
	data, syms, err := loadHistory(ctx, app, cfg.Start.Add(-150*24*time.Hour), end)
	if err != nil {
		return err
	}

	// Benchmarks are pure: no weight cap, stops or kill switch.
	bench := cfg
	bench.Rules.MaxWeight, bench.Rules.StopLoss, bench.Rules.MaxDrawdown = 1, 0, 0
	type run struct {
		label, name, params string
		cfg                 strategy.BacktestConfig
	}
	runs := []run{
		{name, name, *params, cfg},
		{"hold SOL", "hold", `{"weights":{"SOL":1}}`, bench},
		{"50/50 SOL", "rebalance", `{"weights":{"SOL":0.5},"band":0.05}`, bench},
	}
	fmt.Printf("\n  BACKTEST %s → %s (%d days, hourly, $%.0f start, %.0f bps + $%.4f per trade)\n",
		cfg.Start.Format("2006-01-02"), end.Format("2006-01-02"), *days, cfg.InitialUSD, cfg.CostBps, cfg.FeeUSD)
	fmt.Printf("  %-12s %9s %9s %7s %7s %9s %10s\n", "", "return", "max DD", "sharpe", "trades", "costs", "final")
	var main strategy.Result
	for i, r := range runs {
		spec, err := strategy.Lookup(r.name)
		if err != nil {
			return err
		}
		s, _, err := spec.New(json.RawMessage(r.params), app.tokens)
		if err != nil {
			return err
		}
		res, err := strategy.Backtest(s, data, syms, r.cfg)
		if err != nil {
			return err
		}
		if i == 0 {
			main = res
		}
		fmt.Printf("  %-12s %s %8.1f%% %7.2f %7d %9s %10s\n", r.label, colorPct(res.Return*100),
			res.MaxDrawdown*100, res.Sharpe, res.Trades, fmt.Sprintf("$%.2f", res.FeesUSD), fmt.Sprintf("$%.2f", res.FinalValue))
	}
	if main.Halted != "" {
		fmt.Printf("\n  \033[31mkill switch: %s\033[0m\n", main.Halted)
	}
	if n := len(main.Notes); n > 0 {
		fmt.Println("\n  last decisions:")
		for _, note := range main.Notes[max(0, n-5):] {
			fmt.Printf("    %s\n", note)
		}
	}
	fmt.Println()
	return nil
}

// loadHistory loads 1h and 1d candles for all strategy tokens.
func loadHistory(ctx context.Context, app *app, from, to time.Time) (*strategy.MemData, []string, error) {
	syms := app.symbols()
	d, err := strategy.LoadHistory(ctx, app.store, syms, from, to)
	return d, syms, err
}

func (a *app) symbols() []string {
	var out []string
	for _, t := range a.universe() {
		out = append(out, t.Symbol)
	}
	return out
}

func colorPct(p float64) string {
	s := fmt.Sprintf("%+8.1f%%", p)
	switch {
	case p > 0:
		return "\033[32m" + s + "\033[0m"
	case p < 0:
		return "\033[31m" + s + "\033[0m"
	}
	return s
}

func trimDecimals(s string) string {
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil || math.IsNaN(f) {
		return s
	}
	return fmt.Sprintf("%.2f", f)
}

func sortedKeys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
