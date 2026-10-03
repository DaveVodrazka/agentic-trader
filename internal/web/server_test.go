package web

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentic-trader/internal/pnl"
	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

type noVenue struct{}

func (noVenue) Name() string { return "none" }
func (noVenue) Quote(context.Context, venue.QuoteRequest) (*venue.Quote, error) {
	return nil, errors.New("no route")
}

type fakeReviewer struct {
	running bool
	starts  int
}

func (f *fakeReviewer) Running() bool { return f.running }
func (f *fakeReviewer) Start() error {
	if f.running {
		return ErrReviewRunning
	}
	f.running = true
	f.starts++
	return nil
}

func setup(t *testing.T) (http.Handler, *store.Store, *fakeReviewer, *time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	start := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	var toks []store.Token
	for _, tk := range venue.SolanaTokens {
		toks = append(toks, store.Token{Symbol: tk.Symbol, Mint: tk.Address, Decimals: tk.Decimals})
	}
	if err := st.UpsertTokens(ctx, toks); err != nil {
		t.Fatal(err)
	}
	deps := []store.Deposit{{At: start, Symbol: "USDC", Amount: big.NewInt(1000e6), ValueUSDC: "1000"}}
	if err := st.AddDeposit(ctx, deps[0]); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSnapshot(ctx, pnl.InitialSnapshot(deps, tokens)); err != nil {
		t.Fatal(err)
	}
	now := time.Now() // pnl.Take stamps snapshots with the wall clock
	rev := &fakeReviewer{}
	srv := New(Config{Store: st, Tokens: tokens, Venue: noVenue{}, Symbols: []string{"SOL"}, Reviewer: rev,
		Now: func() time.Time { return now }})
	return srv.Handler(), st, rev, &now
}

func do(t *testing.T, h http.Handler, method, path string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "http://localhost:8080"+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return rec.Code, body
}

var write = map[string]string{"X-Trader": "1"}

func TestLocalOnly(t *testing.T) {
	h, _, rev, _ := setup(t)
	req := httptest.NewRequest("GET", "http://evil.example/api/snapshots", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign host: %d", rec.Code)
	}
	for _, host := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost"} {
		req := httptest.NewRequest("GET", "http://"+host+"/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Agentic Trader") {
			t.Errorf("%s: %d", host, rec.Code)
		}
	}
	if code, _ := do(t, h, "POST", "/api/review", nil); code != http.StatusForbidden || rev.starts != 0 {
		t.Errorf("write without header: %d, starts %d", code, rev.starts)
	}
}

func TestSnapshots(t *testing.T) {
	h, _, _, now := setup(t)

	code, latest := do(t, h, "GET", "/api/snapshots/latest", nil)
	if code != 200 || latest["kind"] != "initial" || latest["value_usdc"] != "1000.000000" {
		t.Fatalf("latest: %d %v", code, latest)
	}

	code, taken := do(t, h, "POST", "/api/snapshots", write)
	if code != http.StatusCreated || taken["kind"] != "manual" || taken["prev_id"] != latest["id"] {
		t.Fatalf("take: %d %v", code, taken)
	}
	if hs := taken["holdings"].([]any); len(hs) != 1 || hs[0].(map[string]any)["amount"] != "1000" {
		t.Errorf("holdings = %v", hs)
	}

	// A second click right away returns the same snapshot.
	*now = now.Add(5 * time.Second)
	if code, again := do(t, h, "POST", "/api/snapshots", write); code != 200 || again["id"] != taken["id"] {
		t.Errorf("repeat: %d %v", code, again)
	}
	*now = now.Add(MinSnapshotGap)
	if code, next := do(t, h, "POST", "/api/snapshots", write); code != http.StatusCreated || next["id"] == taken["id"] {
		t.Errorf("after gap: %d %v", code, next)
	}

	_, list := do(t, h, "GET", "/api/snapshots?limit=2", nil)
	if snaps := list["snapshots"].([]any); len(snaps) != 2 || list["more"] != true {
		t.Errorf("list = %v", list)
	}
	_, list = do(t, h, "GET", "/api/snapshots?kind=initial", nil)
	if snaps := list["snapshots"].([]any); len(snaps) != 1 || list["more"] != false {
		t.Errorf("initial = %v", list)
	}
	if code, _ := do(t, h, "GET", "/api/snapshots?kind=bogus", nil); code != http.StatusBadRequest {
		t.Errorf("bad kind: %d", code)
	}
	if code, _ := do(t, h, "GET", "/api/snapshots/999", nil); code != http.StatusNotFound {
		t.Errorf("missing: %d", code)
	}
	if _, eq := do(t, h, "GET", "/api/equity", nil); len(eq["points"].([]any)) != 3 {
		t.Errorf("equity = %v", eq)
	}
}

func TestStrategyNarrativeReview(t *testing.T) {
	h, st, rev, _ := setup(t)
	ctx := context.Background()

	if _, s := do(t, h, "GET", "/api/strategy", nil); s["live"] != false {
		t.Errorf("strategy = %v", s)
	}
	a := store.Activation{Strategy: "cash", Params: "{}", Reason: "test", SetBy: "user", StartedAt: time.Now()}
	if err := st.StartStrategy(ctx, &a); err != nil {
		t.Fatal(err)
	}
	if _, s := do(t, h, "GET", "/api/strategy", nil); s["live"] != true || len(s["history"].([]any)) != 1 {
		t.Errorf("strategy = %v", s)
	}

	if err := st.SaveNarrative(ctx, "run-1", "## Market view\ncalm", "kept cash", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, n := do(t, h, "GET", "/api/narrative", nil)
	if n["narrative"].(map[string]any)["body"] != "## Market view\ncalm" || len(n["journal"].([]any)) != 1 {
		t.Errorf("narrative = %v", n)
	}

	if code, _ := do(t, h, "POST", "/api/review", write); code != http.StatusAccepted || rev.starts != 1 {
		t.Errorf("review: %d", code)
	}
	if code, _ := do(t, h, "POST", "/api/review", write); code != http.StatusConflict || rev.starts != 1 {
		t.Errorf("second review: %d", code)
	}
	_, r := do(t, h, "GET", "/api/review", nil)
	if r["running"] != true || len(r["runs"].([]any)) != 1 {
		t.Errorf("review status = %v", r)
	}
}
