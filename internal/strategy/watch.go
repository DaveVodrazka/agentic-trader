package strategy

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"agentic-trader/internal/store"
)

// WakeRules decide when market events should wake the agent early.
type WakeRules struct {
	MoveHour            float64       // a token moving this fraction within an hour
	DrawdownSinceReview float64       // portfolio falling this fraction since the last review
	StaleAfter          time.Duration // no fresh prices for this long
	Cooldown            time.Duration // the same event fires at most once per cooldown
	MinGap              time.Duration // minimum time between agent runs started by wake-ups
}

// DefaultWakeRules are the live defaults.
var DefaultWakeRules = WakeRules{
	MoveHour:            0.05,
	DrawdownSinceReview: 0.03,
	StaleAfter:          2 * time.Hour,
	Cooldown:            time.Hour,
	MinGap:              15 * time.Minute,
}

// Waker starts an agent review now (e.g. launchctl kickstart).
type Waker interface {
	Wake(ctx context.Context) error
}

// WatchResult reports what a check found.
type WatchResult struct {
	Recorded []store.Wakeup // new wake-ups (after debounce)
	Pending  int            // unhandled wake-ups in total
	Woke     bool           // the agent was started
	Held     string         // why a pending wake-up did not start the agent
}

// CheckWakeups looks for market events after a tick (res may be nil if the
// tick failed), records them as wake-ups, and wakes the agent if any are
// pending and the last run is at least MinGap ago.
func CheckWakeups(ctx context.Context, st *store.Store, res *TickResult, symbols []string, rules WakeRules,
	waker Waker, now time.Time) (WatchResult, error) {
	var out WatchResult
	for _, w := range detect(ctx, st, res, symbols, rules, now) {
		added, err := st.AddWakeup(ctx, w, rules.Cooldown)
		if err != nil {
			return out, err
		}
		if added {
			out.Recorded = append(out.Recorded, w)
		}
	}

	pending, err := st.PendingWakeups(ctx)
	if err != nil || pending == 0 {
		return out, err
	}
	out.Pending = pending
	if last, ok, err := st.LastRunStart(ctx); err != nil {
		return out, err
	} else if ok && now.Sub(last) < rules.MinGap {
		out.Held = fmt.Sprintf("last agent run started %s ago (min gap %s)", now.Sub(last).Round(time.Minute), rules.MinGap)
		return out, nil
	}
	if waker == nil {
		out.Held = "no waker configured; the next scheduled review will handle it"
		return out, nil
	}
	if err := waker.Wake(ctx); err != nil {
		out.Held = "wake failed: " + err.Error()
		return out, nil
	}
	out.Woke = true
	return out, nil
}

func detect(ctx context.Context, st *store.Store, res *TickResult, symbols []string, rules WakeRules, now time.Time) []store.Wakeup {
	var ws []store.Wakeup
	add := func(kind, key, detail string) {
		ws = append(ws, store.Wakeup{At: now, Kind: kind, Key: key, Detail: detail})
	}

	if res == nil {
		// The tick failed (e.g. no prices): wake if data has gone stale.
		var newest time.Time
		for _, sym := range symbols {
			if cs, err := st.Candles(ctx, sym, "1h", now.Add(-48*time.Hour), now.Add(time.Second)); err == nil && len(cs) > 0 {
				if s := cs[len(cs)-1].Start; s.After(newest) {
					newest = s
				}
			}
		}
		if age := now.Sub(newest); !newest.IsZero() && age > rules.StaleAfter {
			add("stale", "stale", fmt.Sprintf("no fresh prices for %s; ticks are failing", age.Round(time.Minute)))
		}
		return ws
	}

	if res.Plan.Halt != "" {
		add("halt", "halt", "strategy halted by the drawdown kill switch: "+res.Plan.Halt)
	}
	for _, sym := range res.Plan.NewStops {
		add("stop", "stop:"+sym, fmt.Sprintf("stop-loss sold %s at $%s", sym, fmtPrice(res.Prices[sym])))
	}

	// Portfolio change since the last review's snapshot.
	if snap, ok, err := st.LatestSnapshot(ctx, store.KindRun); err == nil && ok && res.Portfolio.Total > 0 {
		var base float64
		fmt.Sscanf(snap.ValueUSDC, "%g", &base)
		if base > 0 {
			if chg := res.Portfolio.Total/base - 1; chg <= -rules.DrawdownSinceReview {
				add("drawdown", "drawdown", fmt.Sprintf("portfolio %+.1f%% since last review ($%.2f → $%.2f)",
					chg*100, base, res.Portfolio.Total))
			}
		}
	}

	// Token moves within the last hour, from 5-minute bars.
	syms := append([]string(nil), symbols...)
	sort.Strings(syms)
	for _, sym := range syms {
		price := res.Prices[sym]
		if price <= 0 {
			continue
		}
		cs, err := st.Candles(ctx, sym, "5m", now.Add(-65*time.Minute), now.Add(-55*time.Minute))
		if err != nil || len(cs) == 0 || cs[0].Close <= 0 {
			continue
		}
		then := cs[0].Close
		if chg := price/then - 1; math.Abs(chg) >= rules.MoveHour {
			add("move", "move:"+sym, fmt.Sprintf("%s %+.1f%% in the last hour ($%s → $%s)", sym, chg*100,
				fmtPrice(then), fmtPrice(price)))
		}
	}
	return ws
}
