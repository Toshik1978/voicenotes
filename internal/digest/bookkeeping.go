package digest

import "time"

// RunStatus is the outcome recorded for one local day's digest.
type RunStatus string

// The recorded outcomes of a digest run.
const (
	// StatusSent means a digest message was delivered.
	StatusSent RunStatus = "sent"
	// StatusEmpty means the inbox was empty, so nothing was sent.
	StatusEmpty RunStatus = "empty"
	// StatusSkipped means the catch-up window passed or Memos was unreachable.
	StatusSkipped RunStatus = "skipped"
)

// Run is the bookkeeping row of one local day (FR-027: at most one per day).
type Run struct {
	// LocalDate is the day in the configured time zone (TZ), formatted YYYY-MM-DD.
	LocalDate string
	Status    RunStatus
	// SentAt is when the outcome was recorded.
	SentAt    time.Time
	NoteCount int
}
