package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentic-trader/internal/store"
)

const valid = `## Market view
SOL ranging.

## Active strategy
rebalance 50/50

## Why
Range-bound, low trend.

## Evidence
30d backtest +9% vs hold +10%, lower drawdown.

## Switch if
SOL daily trend turns up with MA20 > MA50.

## Lessons
- none yet`

func newMemory(t *testing.T) (*Memory, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	view := filepath.Join(dir, "NARRATIVE.md")
	return New(st, view, 0), st, view
}

func TestUpdateStoresAndWritesView(t *testing.T) {
	m, st, view := newMemory(t)
	ctx := context.Background()
	if err := m.Update(ctx, "run-1", valid, "bought SOL at range bottom"); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(ctx, "run-2", strings.Replace(valid, "SOL ranging.", "SOL breaking out.", 1), "waited"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(view)
	if !strings.Contains(string(data), "by run run-2") || !strings.Contains(string(data), "SOL breaking out.") {
		t.Errorf("view = %s", data)
	}
	j, _ := st.Journal(ctx, 0)
	if len(j) != 2 || j[0].Summary != "bought SOL at range bottom" {
		t.Errorf("journal = %+v", j)
	}

	c, err := m.Context(ctx, "run-3", false, []store.Wakeup{{At: time.Now(), Detail: "SOL -6.0% in the last hour"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Run ID: run-3", "Woken early", "SOL -6.0% in the last hour", "SOL breaking out.", "run-1: bought SOL", "run-2: waited"} {
		if !strings.Contains(c, want) {
			t.Errorf("context missing %q:\n%s", want, c)
		}
	}
}

func TestContextFirstRun(t *testing.T) {
	m, _, _ := newMemory(t)
	c, err := m.Context(context.Background(), "run-1", false, nil)
	if err != nil || !strings.Contains(c, "first run") || !strings.Contains(c, "(empty)") || !strings.Contains(c, "Routine hourly review") {
		t.Errorf("context = %q, err = %v", c, err)
	}
}

func TestContextRequested(t *testing.T) {
	m, _, _ := newMemory(t)
	c, err := m.Context(context.Background(), "run-1", true, nil)
	if err != nil || !strings.Contains(c, "requested by the owner") || strings.Contains(c, "Routine hourly review") {
		t.Errorf("context = %q, err = %v", c, err)
	}
}

func TestValidation(t *testing.T) {
	m, st, view := newMemory(t)
	cases := []struct {
		narrative, summary string
		want               error
	}{
		{strings.Replace(valid, "## Lessons", "## Notes", 1), "x", ErrMissingSection},
		{valid + strings.Repeat("x", DefaultMaxNarrativeBytes), "x", ErrNarrativeTooLong},
		{valid, "   ", ErrEmptyJournalEntry},
	}
	for _, c := range cases {
		if err := m.Update(context.Background(), "r", c.narrative, c.summary); !errors.Is(err, c.want) {
			t.Errorf("err = %v, want %v", err, c.want)
		}
	}
	if j, _ := st.Journal(context.Background(), 0); len(j) != 0 {
		t.Error("journal written despite validation failure")
	}
	if _, err := os.Stat(view); !errors.Is(err, os.ErrNotExist) {
		t.Error("view written despite validation failure")
	}
}

func TestTemplateIsValid(t *testing.T) {
	m, _, _ := newMemory(t)
	if err := m.validate(Template()); err != nil {
		t.Fatal(err)
	}
}
