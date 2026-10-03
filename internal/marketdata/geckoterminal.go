// Package marketdata fetches historical price bars for backfilling candles.
package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

// DefaultGeckoTerminalURL is GeckoTerminal's public API (no key; strictly
// rate limited, roughly 10 requests per minute in practice).
const DefaultGeckoTerminalURL = "https://api.geckoterminal.com/api/v2"

// GeckoTerminal fetches OHLCV bars for Solana tokens from their most liquid
// pool, priced in USD.
type GeckoTerminal struct {
	BaseURL     string
	HTTP        *http.Client
	Delay       time.Duration // between requests, to respect the rate limit
	RetryAfter  time.Duration // wait after HTTP 429 before retrying
	MaxRetries  int
	OnRateLimit func(wait time.Duration) // optional progress hook
}

// NewGeckoTerminal returns a client with conservative pacing.
func NewGeckoTerminal() *GeckoTerminal {
	return &GeckoTerminal{BaseURL: DefaultGeckoTerminalURL, HTTP: &http.Client{Timeout: 20 * time.Second},
		Delay: 6 * time.Second, RetryAfter: 60 * time.Second, MaxRetries: 3}
}

// errRateLimited marks an HTTP 429.
var errRateLimited = errors.New("rate limited")

func (g *GeckoTerminal) get(ctx context.Context, path string, q url.Values, v any) error {
	for attempt := 0; ; attempt++ {
		err := g.getOnce(ctx, path, q, v)
		if !errors.Is(err, errRateLimited) || attempt >= g.MaxRetries {
			return err
		}
		if g.OnRateLimit != nil {
			g.OnRateLimit(g.RetryAfter)
		}
		select {
		case <-time.After(g.RetryAfter):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *GeckoTerminal) getOnce(ctx context.Context, path string, q url.Values, v any) error {
	u := g.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("geckoterminal %s: %w", path, errRateLimited)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("geckoterminal %s: HTTP %d: %.200s", path, resp.StatusCode, body)
	}
	return json.Unmarshal(body, v)
}

// TopPool returns the address of the token's pool with the most liquidity.
func (g *GeckoTerminal) TopPool(ctx context.Context, mint string) (addr, name string, err error) {
	var resp struct {
		Data []struct {
			Attributes struct {
				Address string `json:"address"`
				Name    string `json:"name"`
				Reserve string `json:"reserve_in_usd"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := g.get(ctx, "/networks/solana/tokens/"+mint+"/pools", url.Values{"page": {"1"}}, &resp); err != nil {
		return "", "", err
	}
	best := -1.0
	for _, p := range resp.Data {
		r, _ := strconv.ParseFloat(p.Attributes.Reserve, 64)
		if r > best {
			best, addr, name = r, p.Attributes.Address, p.Attributes.Name
		}
	}
	if addr == "" {
		return "", "", fmt.Errorf("no pools for %s", mint)
	}
	return addr, name, nil
}

// timeframes maps our intervals to GeckoTerminal's.
var timeframes = map[string]string{"1h": "hour", "1d": "day"}

// OHLCV returns up to limit bars (max 1000) of token's USD price in pool,
// oldest first. If before is non-zero, only bars at or before it are
// returned (paging back in time); otherwise the newest bars.
func (g *GeckoTerminal) OHLCV(ctx context.Context, pool string, tok venue.Token, interval string, limit int, before time.Time) ([]store.Candle, error) {
	tf, ok := timeframes[interval]
	if !ok {
		return nil, fmt.Errorf("unsupported interval %q", interval)
	}
	var resp struct {
		Data struct {
			Attributes struct {
				List [][]float64 `json:"ohlcv_list"`
			} `json:"attributes"`
		} `json:"data"`
	}
	q := url.Values{"aggregate": {"1"}, "limit": {strconv.Itoa(limit)}, "currency": {"usd"}, "token": {tok.Address}}
	if !before.IsZero() {
		q.Set("before_timestamp", strconv.FormatInt(before.Unix(), 10))
	}
	if err := g.get(ctx, "/networks/solana/pools/"+pool+"/ohlcv/"+tf, q, &resp); err != nil {
		return nil, err
	}
	list := resp.Data.Attributes.List
	out := make([]store.Candle, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- { // API returns newest first
		r := list[i]
		if len(r) < 5 || r[4] <= 0 {
			continue
		}
		out = append(out, store.Candle{Symbol: tok.Symbol, Interval: interval, Start: time.Unix(int64(r[0]), 0).UTC(),
			Open: r[1], High: r[2], Low: r[3], Close: r[4], Source: Provider})
	}
	return out, nil
}

// Provider is the source name recorded on bars and backfill state.
const Provider = "geckoterminal"

// Mode selects what a backfill fetches.
type Mode int

const (
	// Incremental fetches only bars missing since the last check (and
	// fills gaps, e.g. while ticks were not running). The default.
	Incremental Mode = iota
	// Full refetches the newest window and replaces stored bars.
	Full
	// Older pages one window further back than the oldest stored bar.
	Older
)

// maxBars per request and interval (the API's page size).
var maxBars = map[string]int{"1h": 1000, "1d": 365}

// BackfillResult reports one token's backfill.
type BackfillResult struct {
	Symbol   string
	Pool     string
	Added    map[string]int    // interval -> new bars stored
	Status   map[string]string // interval -> "up to date", "+N bars", ...
	Requests int
	Err      error
}

// Backfill brings candles for each token up to date per mode. One token
// failing does not stop the others.
func Backfill(ctx context.Context, st *store.Store, g *GeckoTerminal, tokens []venue.Token, mode Mode, now time.Time,
	progress func(BackfillResult)) error {
	b := &backfiller{st: st, g: g, now: now.UTC()}
	for _, tok := range tokens {
		res := b.token(ctx, tok, mode)
		if errors.Is(res.Err, context.Canceled) {
			return res.Err
		}
		progress(res)
	}
	return nil
}

type backfiller struct {
	st      *store.Store
	g       *GeckoTerminal
	now     time.Time
	lastReq time.Time
}

// pace waits so requests are at least Delay apart; the first is immediate.
func (b *backfiller) pace(ctx context.Context) error {
	if !b.lastReq.IsZero() {
		if wait := b.g.Delay - time.Since(b.lastReq); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	b.lastReq = time.Now()
	return nil
}

func (b *backfiller) token(ctx context.Context, tok venue.Token, mode Mode) BackfillResult {
	res := BackfillResult{Symbol: tok.Symbol, Added: map[string]int{}, Status: map[string]string{}}

	// Reuse the known pool unless refetching from scratch.
	var pool, poolName string
	for _, iv := range []string{"1h", "1d"} {
		if s, ok, err := b.st.GetBackfillState(ctx, tok.Symbol, iv); err == nil && ok && mode != Full {
			pool, poolName = s.Pool, s.PoolName
			break
		}
	}
	if pool == "" {
		if err := b.pace(ctx); err != nil {
			res.Err = err
			return res
		}
		res.Requests++
		var err error
		if pool, poolName, err = b.g.TopPool(ctx, tok.Address); err != nil {
			res.Err = err
			return res
		}
	}
	res.Pool = poolName

	for _, iv := range []string{"1h", "1d"} {
		if err := b.interval(ctx, tok, iv, pool, poolName, mode, &res); err != nil {
			res.Err = fmt.Errorf("%s: %w", iv, err)
			return res
		}
	}
	return res
}

func (b *backfiller) interval(ctx context.Context, tok venue.Token, iv, pool, poolName string, mode Mode, res *BackfillResult) error {
	dur := strategyIntervals[iv]
	current := b.now.Truncate(dur) // the bar in progress
	state, hasState, err := b.st.GetBackfillState(ctx, tok.Symbol, iv)
	if err != nil {
		return err
	}
	state.Symbol, state.Interval, state.Provider, state.Pool, state.PoolName = tok.Symbol, iv, Provider, pool, poolName

	var (
		limit  = maxBars[iv]
		before time.Time
		write  = store.ReplaceSameSource
	)
	switch mode {
	case Full:
		write = store.ReplaceAll
	case Older:
		earliest, ok, err := b.st.EarliestCandle(ctx, tok.Symbol, iv)
		if err != nil {
			return err
		}
		if !ok {
			res.Status[iv] = "no data yet; run a normal backfill first"
			return nil
		}
		before, write = earliest, store.InsertMissing
	case Incremental:
		from, ok, err := b.firstMissing(ctx, tok.Symbol, iv, current, state, hasState)
		if err != nil {
			return err
		}
		if !ok {
			res.Status[iv] = "up to date"
			if !hasState || state.CheckedThrough.Before(current) {
				if err := b.recordEarliest(ctx, &state); err != nil {
					return err
				}
				state.CheckedThrough = current
				return b.st.SaveBackfillState(ctx, state, b.now)
			}
			return nil
		}
		// Re-request the last checked bar too: it may have been partial.
		if hasState && !state.CheckedThrough.IsZero() && state.CheckedThrough.Before(from) {
			from = state.CheckedThrough
		}
		limit = min(limit, int(current.Sub(from)/dur)+1)
	}

	if err := b.pace(ctx); err != nil {
		return err
	}
	res.Requests++
	bars, err := b.g.OHLCV(ctx, pool, tok, iv, limit, before)
	if err != nil {
		return err
	}
	beforeCount, err := b.count(ctx, tok.Symbol, iv)
	if err != nil {
		return err
	}
	if err := b.st.UpsertCandles(ctx, bars, write); err != nil {
		return err
	}
	afterCount, err := b.count(ctx, tok.Symbol, iv)
	if err != nil {
		return err
	}
	res.Added[iv] = afterCount - beforeCount

	if err := b.recordEarliest(ctx, &state); err != nil {
		return err
	}
	if mode != Older {
		state.CheckedThrough = current
	}
	switch {
	case mode == Older && res.Added[iv] == 0:
		res.Status[iv] = "no older data available"
	case mode == Older:
		res.Status[iv] = fmt.Sprintf("+%d older bars (from %s)", res.Added[iv], state.Earliest.Format("2006-01-02"))
	default:
		res.Status[iv] = fmt.Sprintf("+%d bars", res.Added[iv])
	}
	return b.st.SaveBackfillState(ctx, state, b.now)
}

// firstMissing finds the oldest missing bar between where checking should
// start and the current bar. Bars older than the provider's earliest, or
// older than the last check (provider gaps), are not considered missing.
func (b *backfiller) firstMissing(ctx context.Context, symbol, iv string, current time.Time, state store.BackfillState, hasState bool) (time.Time, bool, error) {
	dur := strategyIntervals[iv]
	from := current.Add(-time.Duration(maxBars[iv]-1) * dur)
	if hasState {
		if state.Earliest.After(from) {
			from = state.Earliest
		}
		if state.CheckedThrough.After(from) {
			from = state.CheckedThrough
		}
	}
	starts, err := b.st.CandleStarts(ctx, symbol, iv, from)
	if err != nil {
		return time.Time{}, false, err
	}
	have := make(map[int64]bool, len(starts))
	for _, s := range starts {
		have[s.Unix()] = true
	}
	for t := from; !t.After(current); t = t.Add(dur) {
		if !have[t.Unix()] {
			return t, true, nil
		}
	}
	return time.Time{}, false, nil
}

// recordEarliest sets state.Earliest to the oldest stored bar.
func (b *backfiller) recordEarliest(ctx context.Context, state *store.BackfillState) error {
	e, ok, err := b.st.EarliestCandle(ctx, state.Symbol, state.Interval)
	if ok && (state.Earliest.IsZero() || e.Before(state.Earliest)) {
		state.Earliest = e
	}
	return err
}

func (b *backfiller) count(ctx context.Context, symbol, iv string) (int, error) {
	starts, err := b.st.CandleStarts(ctx, symbol, iv, time.Unix(0, 0))
	return len(starts), err
}

// strategyIntervals mirrors the candle intervals (kept here to avoid an
// import cycle with the strategy package).
var strategyIntervals = map[string]time.Duration{"1h": time.Hour, "1d": 24 * time.Hour}
