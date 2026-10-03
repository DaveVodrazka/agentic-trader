// Package web serves a local dashboard: PnL snapshots, the live strategy,
// the agent's narrative, and buttons to take a snapshot or request an agent
// review. It is meant for localhost only and has no authentication; requests
// from other hosts and cross-site writes are refused.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentic-trader/internal/pnl"
	"agentic-trader/internal/store"
	"agentic-trader/internal/strategy"
	"agentic-trader/internal/venue"
)

//go:embed static
var static embed.FS

// MinSnapshotGap is how soon after a manual snapshot another request returns
// it instead of taking a new one (guards against repeated clicks).
const MinSnapshotGap = 10 * time.Second

// ErrReviewRunning means an agent review is already in progress.
var ErrReviewRunning = errors.New("an agent review is already running")

// Reviewer starts agent reviews and reports whether one is running.
type Reviewer interface {
	Running() bool
	Start() error
}

// Config holds the dashboard's dependencies.
type Config struct {
	Store    *store.Store
	Tokens   *venue.TokenRegistry
	Venue    venue.Venue // values holdings for new snapshots
	Symbols  []string    // tokens strategies may trade
	Reviewer Reviewer
	Now      func() time.Time
}

// Server is the dashboard's HTTP handler.
type Server struct {
	cfg  Config
	snap sync.Mutex // one snapshot at a time
}

// New returns a Server for cfg.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg}
}

// Handler returns the dashboard and its JSON API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(ui))
	mux.HandleFunc("GET /api/snapshots", s.listSnapshots)
	mux.HandleFunc("GET /api/snapshots/{id}", s.getSnapshot)
	mux.HandleFunc("POST /api/snapshots", s.takeSnapshot)
	mux.HandleFunc("GET /api/equity", s.equity)
	mux.HandleFunc("GET /api/strategy", s.strategy)
	mux.HandleFunc("GET /api/narrative", s.narrative)
	mux.HandleFunc("GET /api/review", s.reviewStatus)
	mux.HandleFunc("POST /api/review", s.startReview)
	return localOnly(mux)
}

// localOnly refuses requests addressed to anything but localhost (DNS
// rebinding) and writes without the X-Trader header, which a cross-site page
// cannot send without a CORS preflight this server never approves.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Trader") == "" {
			http.Error(w, "missing X-Trader header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---- snapshots -----------------------------------------------------------------

// Snapshot is a PnL snapshot as JSON. Money values are decimal strings.
type Snapshot struct {
	ID             int64     `json:"id"`
	TakenAt        time.Time `json:"taken_at"`
	Kind           string    `json:"kind"`
	RunID          string    `json:"run_id,omitempty"`
	Complete       bool      `json:"complete"`
	Value          string    `json:"value_usdc"`
	Deposits       string    `json:"deposits_usdc"`
	PnL            string    `json:"pnl_usdc"`
	ReturnPct      string    `json:"return_pct"`
	APRPct         string    `json:"apr_pct"`
	APYPct         string    `json:"apy_pct"`
	GrossPnL       string    `json:"gross_pnl_usdc"`
	Costs          string    `json:"costs_usdc"`
	NetworkFeesSOL string    `json:"network_fees_sol"`
	CashPct        string    `json:"cash_pct"`
	Trades         int       `json:"trades"`
	Runs           int       `json:"runs"`
	Holdings       []Holding `json:"holdings,omitempty"`
	PrevID         int64     `json:"prev_id,omitempty"` // among the same kinds as requested
	NextID         int64     `json:"next_id,omitempty"`
}

// Holding is one token in a snapshot.
type Holding struct {
	Symbol    string `json:"symbol"`
	Amount    string `json:"amount"`
	Price     string `json:"price_usdc,omitempty"`
	Value     string `json:"value_usdc,omitempty"`
	WeightPct string `json:"weight_pct,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) toJSON(sn store.Snapshot) Snapshot {
	out := Snapshot{ID: sn.ID, TakenAt: sn.TakenAt, Kind: sn.Kind, RunID: sn.RunID, Complete: sn.Complete,
		Value: sn.ValueUSDC, Deposits: sn.DepositsUSDC, PnL: sn.PnLUSDC, ReturnPct: sn.ReturnPct, APRPct: sn.APRPct,
		APYPct: sn.APYPct, GrossPnL: sn.GrossPnLUSDC, Costs: sn.CostsUSDC, NetworkFeesSOL: sn.NetworkFeesSOL,
		CashPct: sn.CashPct, Trades: sn.TradeCount, Runs: sn.RunCount}
	for _, h := range sn.Holdings {
		amt := h.Amount.String()
		if tok, err := s.cfg.Tokens.Lookup(h.Symbol); err == nil {
			amt = venue.FormatUnits(h.Amount, tok.Decimals)
		}
		out.Holdings = append(out.Holdings, Holding{Symbol: h.Symbol, Amount: amt, Price: h.PriceUSDC,
			Value: h.ValueUSDC, WeightPct: h.WeightPct, Error: h.Error})
	}
	return out
}

// kinds parses ?kind=manual,run (empty for all kinds).
func kinds(r *http.Request) ([]string, error) {
	q := r.URL.Query().Get("kind")
	if q == "" || q == "all" {
		return nil, nil
	}
	var out []string
	for _, k := range strings.Split(q, ",") {
		switch k {
		case store.KindInitial, store.KindRun, store.KindManual:
			out = append(out, k)
		default:
			return nil, fmt.Errorf("unknown snapshot kind %q", k)
		}
	}
	return out, nil
}

// listSnapshots pages through snapshot headers, newest first:
// ?kind=&before=<id>&limit=.
func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	ks, err := kinds(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, err := s.cfg.Store.ListSnapshots(r.Context(), before, limit+1, ks...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	more := len(list) > limit
	if more {
		list = list[:limit]
	}
	out := make([]Snapshot, 0, len(list))
	for _, sn := range list {
		out = append(out, s.toJSON(sn))
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out, "more": more})
}

// getSnapshot returns one snapshot with holdings; {id} may be "latest".
// prev_id/next_id follow ?kind=.
func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	ks, err := kinds(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	var sn store.Snapshot
	var ok bool
	if idStr := r.PathValue("id"); idStr == "latest" {
		sn, ok, err = s.cfg.Store.LatestSnapshot(r.Context(), ks...)
	} else {
		id, perr := strconv.ParseInt(idStr, 10, 64)
		if perr != nil {
			httpError(w, http.StatusBadRequest, fmt.Errorf("bad snapshot id %q", idStr))
			return
		}
		sn, ok, err = s.cfg.Store.GetSnapshot(r.Context(), id)
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		httpError(w, http.StatusNotFound, errors.New("no such snapshot"))
		return
	}
	s.writeSnapshot(w, r.Context(), http.StatusOK, sn, ks)
}

func (s *Server) writeSnapshot(w http.ResponseWriter, ctx context.Context, code int, sn store.Snapshot, ks []string) {
	out := s.toJSON(sn)
	var err error
	if out.PrevID, out.NextID, err = s.cfg.Store.AdjacentSnapshots(ctx, sn.ID, ks...); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, code, out)
}

// takeSnapshot values the wallet at live prices and saves a manual snapshot.
// A request within MinSnapshotGap of the last manual snapshot returns that
// one; a request while one is being taken gets 409.
func (s *Server) takeSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.snap.TryLock() {
		httpError(w, http.StatusConflict, errors.New("a snapshot is already being taken"))
		return
	}
	defer s.snap.Unlock()

	last, ok, err := s.cfg.Store.LatestSnapshot(r.Context(), store.KindManual)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if ok && s.cfg.Now().Sub(last.TakenAt) < MinSnapshotGap {
		s.writeSnapshot(w, r.Context(), http.StatusOK, last, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_, sn, err := pnl.Take(ctx, s.cfg.Store, s.cfg.Venue, s.cfg.Tokens, store.KindManual, "")
	if err != nil {
		httpError(w, http.StatusBadGateway, err)
		return
	}
	full, _, err := s.cfg.Store.GetSnapshot(r.Context(), sn.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	s.writeSnapshot(w, r.Context(), http.StatusCreated, full, nil)
}

// EquityPoint is one point of the portfolio value curve.
type EquityPoint struct {
	ID      int64     `json:"id"`
	TakenAt time.Time `json:"taken_at"`
	Kind    string    `json:"kind"`
	Value   string    `json:"value_usdc"`
	PnL     string    `json:"pnl_usdc"`
}

// equity returns complete snapshots, oldest first, for the value chart.
func (s *Server) equity(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.ListSnapshots(r.Context(), 0, 5000)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]EquityPoint, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		if sn := list[i]; sn.Complete {
			out = append(out, EquityPoint{ID: sn.ID, TakenAt: sn.TakenAt, Kind: sn.Kind, Value: sn.ValueUSDC, PnL: sn.PnLUSDC})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": out})
}

// ---- strategy and narrative ----------------------------------------------------

// Tick is a strategy tick as JSON.
type Tick struct {
	At      time.Time       `json:"at"`
	Value   string          `json:"value_usdc"`
	Targets json.RawMessage `json:"targets"`
	Actions json.RawMessage `json:"actions"`
	Note    string          `json:"note"`
}

// Activation is a past or present strategy activation.
type Activation struct {
	ID         int64     `json:"id"`
	Strategy   string    `json:"strategy"`
	Params     string    `json:"params"`
	Reason     string    `json:"reason"`
	SetBy      string    `json:"set_by"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitzero"`
	Status     string    `json:"status"`
	HaltReason string    `json:"halt_reason,omitempty"`
}

func (s *Server) strategy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	perf, live, err := strategy.LivePerformance(ctx, s.cfg.Store, s.cfg.Symbols, s.cfg.Tokens, s.cfg.Now().UTC())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{"live": live}
	if live {
		out["performance"] = perf
		ticks, err := s.cfg.Store.RecentTicks(ctx, perf.ID, 12)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		ts := make([]Tick, 0, len(ticks))
		for _, t := range ticks {
			ts = append(ts, Tick{At: t.At, Value: t.ValueUSDC, Targets: rawOr(t.Targets, "{}"),
				Actions: rawOr(t.Actions, "[]"), Note: t.Note})
		}
		out["recent_ticks"] = ts
	}
	acts, err := s.cfg.Store.Activations(ctx, 10)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	hist := make([]Activation, 0, len(acts))
	for _, a := range acts {
		hist = append(hist, Activation{ID: a.ID, Strategy: a.Strategy, Params: a.Params, Reason: a.Reason, SetBy: a.SetBy,
			StartedAt: a.StartedAt, EndedAt: a.EndedAt, Status: a.Status, HaltReason: a.HaltReason})
	}
	out["history"] = hist
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) narrative(w http.ResponseWriter, r *http.Request) {
	n, ok, err := s.cfg.Store.LatestNarrative(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	entries, err := s.cfg.Store.Journal(r.Context(), 20)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	type entry struct {
		RunID    string    `json:"run_id"`
		At       time.Time `json:"at"`
		Summary  string    `json:"summary"`
		TradeIDs []string  `json:"trade_ids,omitempty"`
	}
	journal := make([]entry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- { // newest first
		e := entries[i]
		journal = append(journal, entry{RunID: e.RunID, At: e.At, Summary: e.Summary, TradeIDs: e.TradeIDs})
	}
	out := map[string]any{"journal": journal}
	if ok {
		out["narrative"] = map[string]any{"run_id": n.RunID, "at": n.At, "body": n.Body}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- agent reviews -------------------------------------------------------------

// Run is an agent run as JSON.
type Run struct {
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Prompt     string    `json:"prompt,omitempty"`
	Report     string    `json:"report,omitempty"`
}

func (s *Server) reviewStatus(w http.ResponseWriter, r *http.Request) {
	runs, err := s.cfg.Store.Runs(r.Context(), 10)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]Run, 0, len(runs))
	for _, run := range runs {
		out = append(out, Run{ID: run.ID, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, ExitCode: run.ExitCode,
			Prompt: run.Prompt, Report: run.Report})
	}
	running := s.cfg.Reviewer != nil && s.cfg.Reviewer.Running()
	writeJSON(w, http.StatusOK, map[string]any{"running": running, "runs": out})
}

func (s *Server) startReview(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Reviewer == nil {
		httpError(w, http.StatusServiceUnavailable, errors.New("reviews are not configured"))
		return
	}
	if err := s.cfg.Reviewer.Start(); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrReviewRunning) {
			code = http.StatusConflict
		}
		httpError(w, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"running": true})
}

// ---- helpers -------------------------------------------------------------------

func rawOr(s, def string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return json.RawMessage(def)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, err error) {
	if code >= 500 {
		log.Printf("%d: %v", code, err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
