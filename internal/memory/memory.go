// Package memory manages the agent's reasoning across activations: a
// bounded, rewritten narrative (working memory) and an append-only journal
// (history), both stored in the database. The latest narrative is also
// written to a markdown file for humans to read.
package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentic-trader/internal/fsutil"
	"agentic-trader/internal/store"
)

// DefaultNarrativePath is the human-readable copy of the latest narrative.
const DefaultNarrativePath = "NARRATIVE.md"

// DefaultMaxNarrativeBytes keeps the narrative small enough to load every
// run and forces the agent to drop stale reasoning.
const DefaultMaxNarrativeBytes = 4096

// RecentJournalEntries is how many journal entries the agent sees each run
// (about half a day at hourly reviews).
const RecentJournalEntries = 12

// RequiredSections must appear as "## <name>" headings in the narrative.
var RequiredSections = []string{
	"Market view",     // regime: trending, ranging, volatile, risk-off
	"Active strategy", // which one and with what params
	"Why",             // the reasoning for the choice
	"Evidence",        // backtests and live results it rests on
	"Switch if",       // what would make you change strategy
	"Lessons",         // what past choices taught you
}

// Validation errors returned by Update.
var (
	ErrNarrativeTooLong   = errors.New("narrative too long")
	ErrMissingSection     = errors.New("narrative missing required section")
	ErrEmptyJournalEntry  = errors.New("journal entry is required")
	ErrJournalEntryTooBig = errors.New("journal entry too long")
)

const maxJournalBytes = 4096

// Memory validates and stores narratives and journal entries.
type Memory struct {
	st       *store.Store
	viewPath string // "" disables the markdown copy
	maxBytes int
	now      func() time.Time
}

// New returns a Memory over st. viewPath is where the latest narrative is
// mirrored as markdown ("" to disable). maxBytes <= 0 uses the default.
func New(st *store.Store, viewPath string, maxBytes int) *Memory {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxNarrativeBytes
	}
	return &Memory{st: st, viewPath: viewPath, maxBytes: maxBytes, now: time.Now}
}

// MaxBytes is the narrative size limit.
func (m *Memory) MaxBytes() int { return m.maxBytes }

// Update validates and stores a new narrative version and a journal entry
// for runID. The markdown copy is refreshed afterwards; failing to write it
// is reported but the update itself is already saved.
func (m *Memory) Update(ctx context.Context, runID, narrative, summary string) error {
	narrative = strings.TrimSpace(narrative)
	summary = strings.TrimSpace(summary)
	if err := m.validate(narrative); err != nil {
		return err
	}
	if summary == "" {
		return ErrEmptyJournalEntry
	}
	if len(summary) > maxJournalBytes {
		return fmt.Errorf("%w: %d bytes, max %d", ErrJournalEntryTooBig, len(summary), maxJournalBytes)
	}
	at := m.now()
	if err := m.st.SaveNarrative(ctx, runID, narrative, summary, at); err != nil {
		return err
	}
	return m.writeView(store.Narrative{RunID: runID, At: at, Body: narrative})
}

// WriteView refreshes the markdown copy from the latest stored narrative.
func (m *Memory) WriteView(ctx context.Context) error {
	n, ok, err := m.st.LatestNarrative(ctx)
	if err != nil || !ok {
		return err
	}
	return m.writeView(n)
}

func (m *Memory) writeView(n store.Narrative) error {
	if m.viewPath == "" {
		return nil
	}
	header := fmt.Sprintf("<!-- generated from trader.db; edits are ignored. updated %s by run %s -->\n",
		n.At.UTC().Format(time.RFC3339), n.RunID)
	if err := fsutil.WriteFileAtomic(m.viewPath, []byte(header+n.Body+"\n")); err != nil {
		return fmt.Errorf("narrative saved, but writing %s failed: %w", m.viewPath, err)
	}
	return nil
}

// Context renders the block given to the agent at the start of a run: why
// it is running (wake-ups, if any), its narrative and recent journal.
func (m *Memory) Context(ctx context.Context, runID string, requested bool, wakeups []store.Wakeup) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Context\n\nCurrent time: %s\nRun ID: %s\n\n## Why you are running now\n",
		m.now().UTC().Format(time.RFC3339), runID)
	if requested {
		b.WriteString("Review requested by the owner: review the live strategy properly (as when woken early).\n")
	}
	if len(wakeups) == 0 && !requested {
		b.WriteString("Routine hourly review.\n")
	} else if len(wakeups) > 0 {
		b.WriteString("Woken early by market events:\n")
		for _, w := range wakeups {
			fmt.Fprintf(&b, "- %s %s\n", w.At.UTC().Format("15:04Z"), w.Detail)
		}
	}
	b.WriteString("\n## Your narrative\n")
	n, ok, err := m.st.LatestNarrative(ctx)
	if err != nil {
		return "", err
	}
	if ok {
		fmt.Fprintf(&b, "(written %s by %s)\n%s\n", n.At.UTC().Format(time.RFC3339), n.RunID, n.Body)
	} else {
		b.WriteString("(none yet — this is your first run)\n")
	}

	b.WriteString("\n## Recent journal entries (newest last)\n")
	entries, err := m.st.Journal(ctx, RecentJournalEntries)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		b.WriteString("(empty)\n")
	}
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s %s: %s", e.At.UTC().Format(time.RFC3339), e.RunID, e.Summary)
		if len(e.TradeIDs) > 0 {
			fmt.Fprintf(&b, " [trades: %s]", strings.Join(e.TradeIDs, ", "))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (m *Memory) validate(narrative string) error {
	if n := len(narrative); n > m.maxBytes {
		return fmt.Errorf("%w: %d bytes, max %d; compress it and drop stale points", ErrNarrativeTooLong, n, m.maxBytes)
	}
	var missing []string
	for _, sec := range RequiredSections {
		if !hasHeading(narrative, sec) {
			missing = append(missing, "## "+sec)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrMissingSection, strings.Join(missing, ", "))
	}
	return nil
}

func hasHeading(md, name string) bool {
	for line := range strings.Lines(md) {
		if strings.EqualFold(strings.TrimSpace(line), "## "+name) {
			return true
		}
	}
	return false
}

// Template is an empty narrative with the required sections.
func Template() string {
	var b strings.Builder
	for i, sec := range RequiredSections {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## " + sec + "\n")
	}
	return b.String()
}
