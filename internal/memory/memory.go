// Package memory persists the agent's reasoning across activations: a
// bounded, rewritten narrative (working memory) and an append-only journal
// (history).
package memory

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agentic-trader/internal/fsutil"
)

// Default file locations, relative to the working directory.
const (
	DefaultNarrativePath = "NARRATIVE.md"
	DefaultJournalPath   = "journal.jsonl"
	// DefaultMaxNarrativeBytes keeps the narrative small enough to load every
	// run and forces the agent to drop stale reasoning.
	DefaultMaxNarrativeBytes = 4096
)

// RequiredSections must appear as "## <name>" headings in the narrative.
var RequiredSections = []string{
	"Market view",
	"Positions & plan",
	"Next run: watch for",
	"Lessons",
}

// Validation errors returned by Update.
var (
	ErrNarrativeTooLong   = errors.New("narrative too long")
	ErrMissingSection     = errors.New("narrative missing required section")
	ErrEmptyJournalEntry  = errors.New("journal entry is required")
	ErrJournalEntryTooBig = errors.New("journal entry too long")
)

const maxJournalBytes = 4096

// JournalEntry is one line of the journal.
type JournalEntry struct {
	RunID    string    `json:"run_id"`
	At       time.Time `json:"at"`
	Summary  string    `json:"summary"`             // what the agent saw, decided and why
	TradeIDs []string  `json:"trade_ids,omitempty"` // trades executed this run
}

// Store reads and writes the narrative and journal. Safe for concurrent use.
type Store struct {
	mu            sync.Mutex
	narrativePath string
	journalPath   string
	maxBytes      int
	now           func() time.Time
}

// NewStore returns a Store over the given files. maxBytes <= 0 uses the default.
func NewStore(narrativePath, journalPath string, maxBytes int) *Store {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxNarrativeBytes
	}
	return &Store{narrativePath: narrativePath, journalPath: journalPath, maxBytes: maxBytes, now: time.Now}
}

// MaxBytes is the narrative size limit.
func (s *Store) MaxBytes() int { return s.maxBytes }

// Update validates and stores a new narrative, and appends a journal entry.
// The journal is appended first: if the narrative write fails, the run's
// record still survives.
func (s *Store) Update(runID, narrative, summary string, tradeIDs []string) (JournalEntry, error) {
	narrative = strings.TrimSpace(narrative)
	summary = strings.TrimSpace(summary)
	if err := s.validate(narrative); err != nil {
		return JournalEntry{}, err
	}
	if summary == "" {
		return JournalEntry{}, ErrEmptyJournalEntry
	}
	if len(summary) > maxJournalBytes {
		return JournalEntry{}, fmt.Errorf("%w: %d bytes, max %d", ErrJournalEntryTooBig, len(summary), maxJournalBytes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry := JournalEntry{RunID: runID, At: s.now().UTC(), Summary: summary, TradeIDs: tradeIDs}
	line, err := fsutil.MarshalLine(entry)
	if err != nil {
		return JournalEntry{}, err
	}
	if err := fsutil.AppendLine(s.journalPath, line); err != nil {
		return JournalEntry{}, err
	}

	header := fmt.Sprintf("<!-- updated %s by run %s -->\n", entry.At.Format(time.RFC3339), runID)
	if err := fsutil.WriteFileAtomic(s.narrativePath, []byte(header+narrative+"\n")); err != nil {
		return entry, fmt.Errorf("journal saved but narrative write failed: %w", err)
	}
	return entry, nil
}

func (s *Store) validate(narrative string) error {
	if n := len(narrative); n > s.maxBytes {
		return fmt.Errorf("%w: %d bytes, max %d; compress it and drop stale points", ErrNarrativeTooLong, n, s.maxBytes)
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
