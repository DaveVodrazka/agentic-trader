package memory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `## Market view
SOL ranging.

## Positions & plan
- SOL 1 (paper-1). Exit if <115.

## Next run: watch for
- SOL < 117.

## Lessons
- none yet`

func newStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	np, jp := filepath.Join(dir, "NARRATIVE.md"), filepath.Join(dir, "journal.jsonl")
	return NewStore(np, jp, 0), np, jp
}

func TestUpdateWritesNarrativeAndJournal(t *testing.T) {
	s, np, jp := newStore(t)
	if _, err := s.Update("run-1", valid, "bought SOL at range bottom", []string{"paper-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update("run-2", valid, "waited, no setup", nil); err != nil {
		t.Fatal(err)
	}

	n, _ := os.ReadFile(np)
	if !strings.Contains(string(n), "by run run-2") || !strings.Contains(string(n), "## Lessons") {
		t.Errorf("narrative = %s", n)
	}
	lines := strings.Split(strings.TrimSpace(string(must(os.ReadFile(jp)))), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal lines = %d", len(lines))
	}
	var e JournalEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.RunID != "run-1" || e.TradeIDs[0] != "paper-1" {
		t.Errorf("entry = %+v, err = %v", e, err)
	}
}

func TestValidation(t *testing.T) {
	s, np, jp := newStore(t)
	cases := []struct {
		narrative, summary string
		want               error
	}{
		{strings.Replace(valid, "## Lessons", "## Notes", 1), "x", ErrMissingSection},
		{valid + strings.Repeat("x", DefaultMaxNarrativeBytes), "x", ErrNarrativeTooLong},
		{valid, "   ", ErrEmptyJournalEntry},
	}
	for _, c := range cases {
		if _, err := s.Update("r", c.narrative, c.summary, nil); !errors.Is(err, c.want) {
			t.Errorf("err = %v, want %v", err, c.want)
		}
	}
	for _, p := range []string{np, jp} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s written despite validation failure", p)
		}
	}
}

func TestTemplateIsValid(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.validate(Template()); err != nil {
		t.Fatal(err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
