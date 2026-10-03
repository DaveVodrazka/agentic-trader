package marketdata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

// fakeGT serves SOL bars like GeckoTerminal: newest first, up to limit,
// optionally before a timestamp, with history starting at histStart.
type fakeGT struct {
	mu        sync.Mutex
	now       time.Time
	histStart time.Time
	requests  []string
	limit429  int // first n requests are rate limited
}

func (f *fakeGT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limit429 > 0 {
		f.limit429--
		http.Error(w, `{}`, http.StatusTooManyRequests)
		return
	}
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	if strings.HasSuffix(r.URL.Path, "/pools") {
		w.Write([]byte(`{"data":[{"attributes":{"address":"small","name":"SOL / X","reserve_in_usd":"10"}},
			{"attributes":{"address":"big","name":"SOL / USDC","reserve_in_usd":"5000000"}}]}`))
		return
	}
	dur := time.Hour
	if strings.HasSuffix(r.URL.Path, "/day") {
		dur = 24 * time.Hour
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	newest := f.now.Truncate(dur)
	if b := r.URL.Query().Get("before_timestamp"); b != "" {
		sec, _ := strconv.ParseInt(b, 10, 64)
		newest = time.Unix(sec, 0).UTC().Truncate(dur)
	}
	var rows []string
	for t := newest; len(rows) < limit && !t.Before(f.histStart); t = t.Add(-dur) {
		p := float64(t.Unix()%1000) + 1
		rows = append(rows, fmt.Sprintf("[%d,%g,%g,%g,%g,1]", t.Unix(), p, p, p, p))
	}
	fmt.Fprintf(w, `{"data":{"attributes":{"ohlcv_list":[%s]}}}`, strings.Join(rows, ","))
}

func (f *fakeGT) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.requests
	f.requests = nil
	return r
}

func setup(t *testing.T, f *fakeGT) (*store.Store, *GeckoTerminal, venue.Token) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertTokens(context.Background(), []store.Token{{Symbol: "SOL", Mint: "So11111111111111111111111111111111111111112", Decimals: 9}}); err != nil {
		t.Fatal(err)
	}
	sol, _ := venue.NewTokenRegistry(venue.SolanaTokens...).Lookup("SOL")
	return st, &GeckoTerminal{BaseURL: srv.URL, HTTP: srv.Client(), MaxRetries: 2}, sol
}

func run(t *testing.T, st *store.Store, g *GeckoTerminal, tok venue.Token, mode Mode, now time.Time) BackfillResult {
	t.Helper()
	var res BackfillResult
	if err := Backfill(context.Background(), st, g, []venue.Token{tok}, mode, now, func(r BackfillResult) { res = r }); err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	return res
}

func count(t *testing.T, st *store.Store, iv string) int {
	t.Helper()
	s, err := st.CandleStarts(context.Background(), "SOL", iv, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return len(s)
}

func TestIncrementalBackfill(t *testing.T) {
	now := time.Date(2026, 10, 3, 14, 30, 0, 0, time.UTC)
	f := &fakeGT{now: now, histStart: now.Add(-200 * 24 * time.Hour)}
	st, g, sol := setup(t, f)
	ctx := context.Background()

	// First run: pool lookup + full windows.
	res := run(t, st, g, sol, Incremental, now)
	if reqs := f.take(); len(reqs) != 3 || res.Pool != "SOL / USDC" {
		t.Fatalf("first run: %d requests %v, pool %q", len(reqs), reqs, res.Pool)
	}
	if count(t, st, "1h") != 1000 || count(t, st, "1d") != 200 { // history starts 14:30, 200 days ago
		t.Fatalf("bars: 1h=%d 1d=%d", count(t, st, "1h"), count(t, st, "1d"))
	}

	if st1, ok, _ := st.GetBackfillState(ctx, "SOL", "1h"); !ok || !st1.Earliest.Equal(now.Truncate(time.Hour).Add(-999*time.Hour)) {
		t.Errorf("state = %+v", st1)
	}

	// Same time again: nothing to do, no requests (pool is cached).
	res = run(t, st, g, sol, Incremental, now)
	if reqs := f.take(); len(reqs) != 0 || res.Status["1h"] != "up to date" || res.Status["1d"] != "up to date" {
		t.Fatalf("second run: %v %+v", reqs, res.Status)
	}

	// 5 hours later: one small hourly request (from the last checked bar).
	f.now = now.Add(5 * time.Hour)
	res = run(t, st, g, sol, Incremental, f.now)
	reqs := f.take()
	if len(reqs) != 1 || !strings.Contains(reqs[0], "/hour") || !strings.Contains(reqs[0], "limit=6") || res.Added["1h"] != 5 {
		t.Fatalf("catch-up: %v added=%v", reqs, res.Added)
	}

	// Ticks then record bars themselves, the Mac sleeps for two hours (no
	// bars), and ticks resume. Backfill fills the sleep gap and keeps the
	// tick-built bars.
	h := f.now.Truncate(time.Hour) // last checked bar
	tickBar := func(at time.Time) store.Candle {
		return store.Candle{Symbol: "SOL", Interval: "1h", Start: at, Open: 7, High: 7, Low: 7, Close: 7, Source: "jupiter"}
	}
	must(t, st.UpsertCandles(ctx, []store.Candle{tickBar(h.Add(time.Hour)), tickBar(h.Add(2 * time.Hour)), tickBar(h.Add(5 * time.Hour))}, store.InsertMissing))
	f.now = h.Add(5*time.Hour + 30*time.Minute)
	res = run(t, st, g, sol, Incremental, f.now)
	cs, _ := st.Candles(ctx, "SOL", "1h", h.Add(time.Hour), f.now)
	if len(cs) != 5 || res.Added["1h"] != 2 {
		t.Fatalf("after sleep gap: added %v, bars %+v", res.Added, cs)
	}
	for i, c := range cs {
		wantTick := i == 0 || i == 1 || i == 4
		if (c.Source == "jupiter") != wantTick {
			t.Errorf("bar %s source %s", c.Start.Format("15:04"), c.Source)
		}
	}

	// A hole the provider doesn't have (before the last check) is not
	// re-requested on every run.
	must(t, st.DeleteCandle(ctx, "SOL", "1h", h.Add(-3*time.Hour)))
	f.take()
	res = run(t, st, g, sol, Incremental, f.now)
	if reqs := f.take(); len(reqs) != 0 || res.Status["1h"] != "up to date" {
		t.Errorf("provider hole refetched: %v %+v", reqs, res.Status)
	}
	f.take()
}

func TestOlderAndFull(t *testing.T) {
	now := time.Date(2026, 10, 3, 14, 30, 0, 0, time.UTC)
	f := &fakeGT{now: now, histStart: now.Add(-60 * 24 * time.Hour)}
	st, g, sol := setup(t, f)
	run(t, st, g, sol, Incremental, now)
	f.take()

	res := run(t, st, g, sol, Older, now)
	reqs := f.take()
	if len(reqs) != 2 || !strings.Contains(reqs[0], "before_timestamp=") {
		t.Fatalf("older requests: %v", reqs)
	}
	// 60 days of hourly history from 14:30 = 1440 whole bars; first window had 1000.
	if res.Added["1h"] != 440 || count(t, st, "1h") != 1440 {
		t.Errorf("older: added %v, total %d", res.Added, count(t, st, "1h"))
	}
	res = run(t, st, g, sol, Older, now)
	if res.Status["1h"] != "no older data available" {
		t.Errorf("exhausted: %+v", res.Status)
	}
	f.take()

	res = run(t, st, g, sol, Full, now)
	if reqs := f.take(); len(reqs) != 3 { // pool lookup again + both intervals
		t.Errorf("full: %v", reqs)
	}
}

func TestRateLimitRetry(t *testing.T) {
	now := time.Date(2026, 10, 3, 14, 30, 0, 0, time.UTC)
	f := &fakeGT{now: now, histStart: now.Add(-3 * 24 * time.Hour), limit429: 1}
	st, g, sol := setup(t, f)
	retries := 0
	g.OnRateLimit = func(time.Duration) { retries++ }
	run(t, st, g, sol, Incremental, now)
	if retries != 1 || count(t, st, "1h") != 72 {
		t.Errorf("retries=%d bars=%d", retries, count(t, st, "1h"))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
