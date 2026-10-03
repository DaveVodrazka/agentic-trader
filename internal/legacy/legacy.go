// Package legacy imports the pre-database state files (WALLET.json,
// INITIAL_WALLET.json, trades.jsonl, journal.jsonl, NARRATIVE.md, logs/)
// into the store.
package legacy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

// Files are the legacy state files, relative to the project directory.
var Files = []string{"INITIAL_WALLET.json", "WALLET.json", "trades.jsonl", "journal.jsonl", "NARRATIVE.md", "WALLET.json.lock"}

// Present reports whether dir contains legacy state to import.
func Present(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "INITIAL_WALLET.json"))
	return err == nil
}

// Summary counts what was imported.
type Summary struct {
	Deposits, Trades, Journal, Narratives, Runs int
}

// Import loads the legacy files in dir into st, which must be empty, and
// verifies the resulting balances against WALLET.json.
func Import(ctx context.Context, st *store.Store, tokens *venue.TokenRegistry, dir string) (Summary, error) {
	var sum Summary

	// Runs first (from logs), so trades/journal can reference them.
	runs, err := importRuns(ctx, st, filepath.Join(dir, "logs"))
	if err != nil {
		return sum, err
	}
	sum.Runs = runs

	n, err := importDeposits(ctx, st, tokens, filepath.Join(dir, "INITIAL_WALLET.json"))
	if err != nil {
		return sum, err
	}
	sum.Deposits = n

	if sum.Trades, err = importTrades(ctx, st, tokens, filepath.Join(dir, "trades.jsonl")); err != nil {
		return sum, err
	}
	if sum.Journal, err = importJournal(ctx, st, filepath.Join(dir, "journal.jsonl")); err != nil {
		return sum, err
	}
	if sum.Narratives, err = importNarrative(ctx, st, filepath.Join(dir, "NARRATIVE.md")); err != nil {
		return sum, err
	}
	if err := verify(ctx, st, tokens, filepath.Join(dir, "WALLET.json")); err != nil {
		return sum, err
	}
	n, err = st.CountRuns(ctx)
	sum.Runs = n
	return sum, err
}

func importDeposits(ctx context.Context, st *store.Store, tokens *venue.TokenRegistry, path string) (int, error) {
	var w struct {
		StartedAt time.Time         `json:"started_at"`
		ValueUSDC string            `json:"value_usdc"`
		Balances  map[string]string `json:"balances"`
	}
	if err := readJSON(path, &w); err != nil {
		return 0, err
	}
	syms := make([]string, 0, len(w.Balances))
	for s := range w.Balances {
		syms = append(syms, s)
	}
	// USDC first so it carries the recorded starting value.
	sort.Slice(syms, func(i, j int) bool { return syms[i] == "USDC" || (syms[j] != "USDC" && syms[i] < syms[j]) })
	n := 0
	for i, sym := range syms {
		tok, err := tokens.Lookup(sym)
		if err != nil {
			return n, err
		}
		amt, err := parseAmount(w.Balances[sym], tok.Decimals)
		if err != nil {
			return n, fmt.Errorf("%s %s: %w", path, sym, err)
		}
		if amt.Sign() == 0 {
			continue
		}
		value := "0"
		if i == 0 {
			value = w.ValueUSDC // the file records one value for the whole wallet
		}
		if err := st.AddDeposit(ctx, store.Deposit{At: w.StartedAt, Symbol: tok.Symbol, Amount: amt, ValueUSDC: value,
			Note: "imported from INITIAL_WALLET.json"}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

type legacyTrade struct {
	Seq       int64     `json:"seq"`
	ID        string    `json:"id"`
	Venue     string    `json:"venue"`
	Paper     bool      `json:"paper"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	In        string    `json:"in"`
	Out       string    `json:"out"`
	Price     string    `json:"price"`
	At        time.Time `json:"at"`
	RunID     string    `json:"run_id"`
	Reason    string    `json:"reason"`
	QuotedOut string    `json:"quoted_out"`
	Fees      *struct {
		NetworkSOL     string `json:"network_sol"`
		PlatformFee    string `json:"platform_fee"`
		PriceImpactPct string `json:"price_impact_pct"`
		NetworkUSDC    string `json:"network_usdc"`
		PlatformUSDC   string `json:"platform_usdc"`
		ImpactUSDC     string `json:"impact_usdc"`
	} `json:"fees"`
}

func importTrades(ctx context.Context, st *store.Store, tokens *venue.TokenRegistry, path string) (int, error) {
	var recs []legacyTrade
	if err := readJSONL(path, func(line []byte) error {
		var r legacyTrade
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		recs = append(recs, r)
		return nil
	}); err != nil {
		return 0, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
	for i, r := range recs {
		from, err := tokens.Lookup(r.From)
		if err != nil {
			return i, err
		}
		to, err := tokens.Lookup(r.To)
		if err != nil {
			return i, err
		}
		t := &store.Trade{ID: r.ID, RunID: r.RunID, At: r.At, Venue: r.Venue, Paper: r.Paper, From: from.Symbol, To: to.Symbol,
			Price: r.Price, Reason: r.Reason}
		if t.InAmount, err = parseAmount(r.In, from.Decimals); err != nil {
			return i, fmt.Errorf("trade %s in: %w", r.ID, err)
		}
		if t.OutAmount, err = parseAmount(r.Out, to.Decimals); err != nil {
			return i, fmt.Errorf("trade %s out: %w", r.ID, err)
		}
		if r.QuotedOut != "" {
			if t.QuotedOut, err = parseAmount(r.QuotedOut, to.Decimals); err != nil {
				return i, fmt.Errorf("trade %s quoted_out: %w", r.ID, err)
			}
		}
		if f := r.Fees; f != nil {
			lamports, err := parseAmount(f.NetworkSOL, 9)
			if err != nil {
				return i, fmt.Errorf("trade %s network_sol: %w", r.ID, err)
			}
			t.Fees = &store.TradeFees{NetworkLamports: lamports.Int64(), PriceImpactPct: f.PriceImpactPct,
				NetworkUSDC: f.NetworkUSDC, PlatformUSDC: f.PlatformUSDC, ImpactUSDC: f.ImpactUSDC}
			if f.PlatformFee != "" {
				if t.Fees.PlatformFee, err = parseAmount(f.PlatformFee, to.Decimals); err != nil {
					return i, fmt.Errorf("trade %s platform_fee: %w", r.ID, err)
				}
			}
		}
		if err := st.RecordTrade(ctx, t); err != nil {
			return i, fmt.Errorf("trade %s (seq %d): %w", r.ID, r.Seq, err)
		}
	}
	return len(recs), nil
}

func importJournal(ctx context.Context, st *store.Store, path string) (int, error) {
	n := 0
	err := readJSONL(path, func(line []byte) error {
		var e struct {
			RunID   string    `json:"run_id"`
			At      time.Time `json:"at"`
			Summary string    `json:"summary"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		n++
		return st.AddJournal(ctx, e.RunID, e.Summary, e.At)
	})
	return n, err
}

var narrativeHeader = regexp.MustCompile(`^<!-- updated (\S+) by run (\S+) -->\n`)

func importNarrative(ctx context.Context, st *store.Store, path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || len(strings.TrimSpace(string(data))) == 0 {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	body, runID, at := string(data), "legacy", time.Now()
	if m := narrativeHeader.FindStringSubmatch(body); m != nil {
		body = body[len(m[0]):]
		runID = m[2]
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			at = t
		}
	}
	return 1, st.AddNarrative(ctx, runID, strings.TrimSpace(body), at)
}

var (
	logStarted  = regexp.MustCompile(`^== (\S+) started (\S+)`)
	logFinished = regexp.MustCompile(`^== (\S+) finished (\S+) exit=(\d+)`)
)

// importRuns creates a run per logs/run-*.log with its timing, exit code and
// output.
func importRuns(ctx context.Context, st *store.Store, dir string) (int, error) {
	paths, _ := filepath.Glob(filepath.Join(dir, "run-*.log"))
	sort.Strings(paths)
	n := 0
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return n, err
		}
		r := store.Run{ID: strings.TrimSuffix(filepath.Base(p), ".log"), LogPath: p}
		var report []string
		for _, line := range strings.Split(string(data), "\n") {
			if m := logStarted.FindStringSubmatch(line); m != nil {
				r.StartedAt, _ = time.Parse(time.RFC3339, m[2])
				continue
			}
			if m := logFinished.FindStringSubmatch(line); m != nil {
				r.FinishedAt, _ = time.Parse(time.RFC3339, m[2])
				code, _ := strconv.Atoi(m[3])
				r.ExitCode = &code
				continue
			}
			report = append(report, line)
		}
		if r.StartedAt.IsZero() {
			continue // not a run log we recognise
		}
		r.Report = strings.TrimSpace(strings.Join(report, "\n"))
		if r.FinishedAt.IsZero() {
			if err := st.BeginRun(ctx, r.ID, "", r.StartedAt); err != nil {
				return n, err
			}
		} else if err := st.EndRun(ctx, r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// verify checks imported balances match WALLET.json.
func verify(ctx context.Context, st *store.Store, tokens *venue.TokenRegistry, path string) error {
	var w struct {
		Balances map[string]string `json:"balances"`
	}
	if err := readJSON(path, &w); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	got, err := st.Balances(ctx)
	if err != nil {
		return err
	}
	for sym, n := range got {
		if _, ok := w.Balances[sym]; !ok && n.Sign() != 0 {
			return fmt.Errorf("imported %s balance %s is not in WALLET.json; not importing", sym, n)
		}
	}
	for sym, s := range w.Balances {
		tok, err := tokens.Lookup(sym)
		if err != nil {
			return err
		}
		want, err := parseAmount(s, tok.Decimals)
		if err != nil {
			return err
		}
		have := got[tok.Symbol]
		if have == nil {
			have = new(big.Int)
		}
		if have.Cmp(want) != 0 {
			return fmt.Errorf("imported %s balance %s does not match WALLET.json %s; not importing",
				sym, venue.FormatUnits(have, tok.Decimals), s)
		}
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func readJSONL(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for line := 1; sc.Scan(); line++ {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		if err := fn(sc.Bytes()); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
	}
	return sc.Err()
}

// parseAmount parses a decimal amount, allowing zero.
func parseAmount(s string, decimals uint8) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s != "" && strings.Trim(s, "0.") == "" {
		return new(big.Int), nil
	}
	return venue.ParseUnits(s, decimals)
}
