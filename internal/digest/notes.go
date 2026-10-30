// Package digest composes and schedules the morning reminder of unreviewed
// notes. This file holds the types the Memos adapter fills in, so the digest
// never imports the adapter.
package digest

import (
	"context"
	"time"
)

// NoteSummary is one inbox note as read back from Memos.
type NoteSummary struct {
	// Name is the Memos resource name, "memos/<uid>".
	Name string
	// URL is the link shown to the owner.
	URL string
	// Content is the note's markdown. It is user data and is never logged.
	Content string
	// CreateTime is when the note was created.
	CreateTime time.Time
	// HasTaskList reports whether the note carries a checklist.
	HasTaskList bool
}

// NotesReader lists the notes still tagged with the inbox tag, oldest first.
type NotesReader interface {
	ListInbox(ctx context.Context) ([]NoteSummary, error)
}
