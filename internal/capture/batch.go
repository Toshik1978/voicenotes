package capture

import (
	"strings"
	"time"
)

// BatchItem is one incoming photo, document or video, reduced to what the
// batching rule needs. Audio and text never batch (FR-106) and never reach
// JoinBatch.
type BatchItem struct {
	// AlbumID is the Telegram media group, if any.
	AlbumID string
	// Caption is the user's text sent with the item.
	Caption string
}

// BatchDecision is the outcome of JoinBatch. NextAttemptAt is the single
// source of truth for the batch's deadline (now + quiet), whether it joined
// or is new.
type BatchDecision struct {
	// Join is true when the item joins the open batch, false for a new one.
	Join bool
	// NextAttemptAt is the batch's new collect deadline: now + quiet.
	NextAttemptAt time.Time
	// DeadlineMoved is true when a worker wake is needed: always for a new
	// batch, and on a join when the deadline moved later than before.
	DeadlineMoved bool
	// Position is the file's position in the batch: max existing + 1 on a
	// join, 0 for a new batch.
	Position int
	// Text is the batch text after the item's caption: the open text with a
	// non-empty caption appended after a blank line, or just the caption for
	// a new batch.
	Text string
}

// JoinBatch decides whether item joins open, the chat's newest files capture
// still at collect (nil if none). It joins when open is an unpicked files
// batch and either its deadline is still ahead of now or item and open share
// a non-empty album ID (late album members). Pure: no I/O.
func JoinBatch(open *Capture, item BatchItem, now time.Time, quiet time.Duration) BatchDecision {
	if strings.TrimSpace(item.Caption) == "" {
		// A blank caption is no text: it must not add an empty paragraph.
		item.Caption = ""
	}
	next := now.Add(quiet)
	if !joins(open, item, now) {
		return BatchDecision{NextAttemptAt: next, DeadlineMoved: true, Text: item.Caption}
	}

	return BatchDecision{
		Join:          true,
		NextAttemptAt: next,
		DeadlineMoved: next.After(open.NextAttemptAt),
		Position:      nextPosition(open.Files),
		Text:          appendCaption(open.Text, item.Caption),
	}
}

// joins reports whether item belongs to the open batch.
func joins(open *Capture, item BatchItem, now time.Time) bool {
	if open == nil || open.Kind != KindFiles || open.Step != StepCollect {
		return false
	}
	sameAlbum := item.AlbumID != "" && item.AlbumID == open.AlbumID

	return open.NextAttemptAt.After(now) || sameAlbum
}

// nextPosition is one past the highest position in files, 0 for none.
func nextPosition(files []File) int {
	pos := 0
	for i := range files {
		pos = max(pos, files[i].Position+1)
	}

	return pos
}

// appendCaption adds a non-empty caption to text after a blank line.
func appendCaption(text, caption string) string {
	switch {
	case caption == "":
		return text
	case text == "":
		return caption
	}

	return text + "\n\n" + caption
}

// AppendResult is what appending one file item to the queue did: the stored
// batch, whether it was created, and whether its deadline moved later
// (always true when created). Ingest acks only a created batch and wakes the
// worker only when Created or DeadlineMoved.
type AppendResult struct {
	Capture       Capture
	Created       bool
	DeadlineMoved bool
}
