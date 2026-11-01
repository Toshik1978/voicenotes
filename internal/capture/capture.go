// Package capture is the pure model of one incoming thought and the step
// state machine that carries it to a note. It performs no I/O: time and
// randomness are injected, and the SQLite store and worker build on it.
package capture

import (
	"fmt"
	"time"
)

// Kind is the type of an incoming capture.
type Kind string

// Capture kinds.
const (
	KindAudio Kind = "audio"
	KindText  Kind = "text"
	// KindFiles is a batch of photos, documents or videos sent together.
	KindFiles Kind = "files"
)

func (k Kind) String() string { return string(k) }

// Step is the next pipeline step to run for a capture.
type Step string

// Pipeline steps, in order.
const (
	StepCollect       Step = "collect"
	StepDownload      Step = "download"
	StepExtract       Step = "extract"
	StepEnrich        Step = "enrich"
	StepWriteNote     Step = "write_note"
	StepFinalizeReply Step = "finalize_reply"
	StepCleanup       Step = "cleanup"
	StepDone          Step = "done"
)

func (s Step) String() string { return string(s) }

// next returns the step that follows s on success; done is terminal.
func (s Step) next() Step {
	switch s {
	case StepCollect:
		return StepDownload
	case StepDownload:
		return StepExtract
	case StepExtract:
		return StepEnrich
	case StepEnrich:
		return StepWriteNote
	case StepWriteNote:
		return StepFinalizeReply
	case StepFinalizeReply:
		return StepCleanup
	case StepCleanup, StepDone:
		return StepDone
	}

	return StepDone
}

// Status is the scheduling state of a capture.
type Status string

// Capture statuses.
const (
	StatusPending Status = "pending"
	StatusWaiting Status = "waiting"
	StatusDone    Status = "done"
)

func (s Status) String() string { return string(s) }

// WaitingOn names the dependency a waiting capture is blocked on. It feeds
// the reply text and the health report. The optional LLM step has none: it
// never surfaces a waiting dependency to the user.
type WaitingOn string

// Dependencies a capture can wait on.
const (
	WaitingTelegram WaitingOn = "telegram"
	WaitingSTT      WaitingOn = "stt"
	WaitingMemos    WaitingOn = "memos"
	WaitingDocling  WaitingOn = "docling"
)

// LLMSkipped reasons, stored in Capture.LLMSkipped.
const (
	SkippedDisabled      = "disabled"
	SkippedUnavailable   = "unavailable"
	SkippedInvalidOutput = "invalid_output"
)

// Capture holds the state the pure logic reads and writes, plus the identity
// and content fields that only adapters touch. Optional (NULLable) columns
// use the zero value for "absent".
type Capture struct {
	// ID is the arrival order assigned by the store on insert.
	ID              int64
	Source          string
	SourceChatID    int64
	SourceMessageID int64

	// AlbumID is the Telegram media group of a files batch's first album item.
	AlbumID string
	// Files are the capture's attachments in send order: the one audio file
	// of an audio capture, or the items of a files batch.
	Files []File
	// Text is the original text or the transcript.
	Text string
	// Language is the ISO-639-1 code; empty if unknown.
	Language string
	// EnrichedJSON is the validated LLM result.
	EnrichedJSON string
	// NoteName and NoteURL identify the written Memos note.
	NoteName  string
	NoteURL   string
	TaskCount int
	// ReplyMessageID is the reply to edit; 0 until a reply has been sent.
	ReplyMessageID int64

	Kind       Kind
	ReceivedAt time.Time

	Step          Step
	Status        Status
	WaitingOn     WaitingOn
	Attempts      int
	NextAttemptAt time.Time
	UpdatedAt     time.Time

	// Failure is the permanent reason that produced a placeholder note.
	Failure string
	// LLMSkipped is why the LLM step was skipped, if it was.
	LLMSkipped string

	// Misconfig is true while the latest outcome was a misconfiguration
	// (retried, never a placeholder). Health reports it as degradation
	// together with WaitingOn and LastError.
	Misconfig bool
	// LastError is the reason of the latest non-OK outcome.
	LastError string

	// QueueReplyEdit is set by the transition that first moves the capture
	// into waiting: the adapter edits the reply to the "queued" text.
	QueueReplyEdit bool
	// MinimalNote asks write_note for a minimal note (placeholder text plus
	// #inbox, no attachment) after Memos rejected the full payload.
	MinimalNote bool
	// SendNewReply asks the adapter to send a new silent reply because the
	// original could not be edited; set when finalize_reply fails permanently.
	SendNewReply bool
	// EnrichStartedAt is when the LLM budget started counting.
	EnrichStartedAt time.Time
}

// New returns a pending capture starting at start (Ruling R1): a files batch
// starts at collect, audio already downloaded at ingest starts at extract,
// audio without a local file at download, and text at enrich.
func New(kind Kind, start Step, receivedAt time.Time) (Capture, error) {
	var valid bool

	switch kind {
	case KindFiles:
		valid = start == StepCollect
	case KindAudio:
		valid = start == StepDownload || start == StepExtract
	case KindText:
		valid = start == StepEnrich
	default:
		return Capture{}, fmt.Errorf("capture: unknown kind %q", kind)
	}
	if !valid {
		return Capture{}, fmt.Errorf("capture: %s capture cannot start at step %q", kind, start)
	}

	return Capture{
		Kind:          kind,
		ReceivedAt:    receivedAt,
		Step:          start,
		Status:        StatusPending,
		NextAttemptAt: receivedAt,
		UpdatedAt:     receivedAt,
	}, nil
}
