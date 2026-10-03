package legacy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var toks []store.Token
	for _, tk := range venue.SolanaTokens {
		toks = append(toks, store.Token{Symbol: tk.Symbol, Mint: tk.Address, Decimals: tk.Decimals})
	}
	if err := st.UpsertTokens(context.Background(), toks); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestImport(t *testing.T) {
	st, ctx := openStore(t), context.Background()
	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	if !Present("testdata") {
		t.Fatal("Present = false")
	}
	sum, err := Import(ctx, st, tokens, "testdata")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Deposits != 1 || sum.Trades != 2 || sum.Journal != 2 || sum.Narratives != 1 {
		t.Errorf("summary = %+v", sum)
	}
	// Runs: one from logs, plus placeholders for runs seen only in trades/journal/narrative.
	if sum.Runs != 3 {
		t.Errorf("runs = %d", sum.Runs)
	}

	trades, _ := st.Trades(ctx)
	if trades[0].Fees != nil || trades[1].Fees == nil || trades[1].Fees.NetworkLamports != 105000 || trades[0].Reason != "core SOL position; exit < $107" {
		t.Errorf("trades = %+v / %+v", trades[0], trades[1].Fees)
	}
	n, ok, _ := st.LatestNarrative(ctx)
	if !ok || n.RunID != "run-20261003T123900Z" || strings.Contains(n.Body, "<!--") || !strings.HasPrefix(n.Body, "## Market view") {
		t.Errorf("narrative = %+v", n)
	}
	j, _ := st.Journal(ctx, 0)
	if len(j) != 2 || len(j[1].TradeIDs) != 1 || j[1].TradeIDs[0] != "paper-aaa" {
		t.Errorf("journal = %+v", j)
	}
}

func TestImportRejectsBalanceMismatch(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"INITIAL_WALLET.json", "trades.jsonl"} {
		data, _ := os.ReadFile(filepath.Join("testdata", f))
		os.WriteFile(filepath.Join(dir, f), data, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "WALLET.json"), []byte(`{"balances":{"USDC":"999","SOL":"3.350297433","WIF":"400.437348"}}`), 0o644)
	_, err := Import(context.Background(), openStore(t), venue.NewTokenRegistry(venue.SolanaTokens...), dir)
	if err == nil || !strings.Contains(err.Error(), "does not match WALLET.json") {
		t.Fatalf("err = %v", err)
	}
}
