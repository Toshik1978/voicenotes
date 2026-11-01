package capture

import (
	"time"
	"unicode"
)

// FileKind is the type of one file in a capture.
type FileKind string

// File kinds.
const (
	FileAudio    FileKind = "audio"
	FileImage    FileKind = "image"
	FileDocument FileKind = "document"
	FileVideo    FileKind = "video"
)

func (k FileKind) String() string { return string(k) }

// FileState is the extraction state of one file.
type FileState string

// File states.
const (
	FilePending FileState = "pending"
	FileDone    FileState = "done"
	FileEmpty   FileState = "empty"
	FileFailed  FileState = "failed"
	FileSkipped FileState = "skipped"
)

func (s FileState) String() string { return string(s) }

// TextSource names what produced a file's text.
type TextSource string

// Text sources.
const (
	TextSTT     TextSource = "stt"
	TextDocling TextSource = "docling"
	TextVision  TextSource = "vision"
)

// File is one file of a capture (data-model.md "Capture file"). Optional
// (NULLable) columns use the zero value for "absent".
type File struct {
	// ID and CaptureID are assigned by the store.
	ID        int64
	CaptureID int64
	// Position is the 0-based order in the send.
	Position int

	// Source, SourceChatID and SourceMessageID identify the item for dedupe.
	Source          string
	SourceChatID    int64
	SourceMessageID int64

	Kind FileKind
	// FileID is the Telegram file ID; cleared at cleanup.
	FileID   string
	MimeType string
	// FileName is the original name, if any; cleared at cleanup.
	FileName string
	FileSize int64
	// LocalPath is relative to the files directory; empty when no file is held.
	LocalPath string

	State FileState
	// Text is the extracted text or transcript; cleared at cleanup.
	Text       string
	TextSource TextSource
	// Language is the detected language (audio only).
	Language string
	// Failure is the secret-free reason for the failed state.
	Failure string
	// Timeouts counts docling timeouts on this file.
	Timeouts int
	// VisionAttempts counts failed vision-model attempts on this file;
	// VisionStartedAt is when the first one happened (the 2-minute budget
	// counts from it).
	VisionAttempts  int
	VisionStartedAt time.Time
	// Attached is true once attached to the note (or the attach failed for good).
	Attached      bool
	AttachFailure string
}

// maxDoclingTimeouts is how many docling timeouts one file may take before
// extraction gives up on it (FR-113).
const maxDoclingTimeouts = 3

// failureTimeout is the placeholder reason for a file that timed out too often.
const failureTimeout = "couldn't read in time"

// DoclingTimeout records one docling timeout on f and returns its outcome. The
// third timeout on the same file fails the file for good with the
// "couldn't read in time" reason (Permanent). Earlier timeouts are Temporary
// and wait on docling. An unreachable docling service is not a timeout and is
// classified by the HTTP client instead.
func DoclingTimeout(f *File) Outcome {
	f.Timeouts++
	if f.Timeouts >= maxDoclingTimeouts {
		f.State = FileFailed
		f.Failure = failureTimeout
		return Permanent(failureTimeout)
	}

	return Temporary("docling timeout").On(WaitingDocling)
}

// Vision budget (contracts/external-apis.md section 2): a file gets 3
// attempts or 2 minutes at the vision model, then it is left empty.
const (
	visionMaxAttempts = 3
	visionMaxDuration = 2 * time.Minute
	// visionMinRunes is how many non-space runes docling must have read for a
	// photo to skip the vision model (FR-121).
	visionMinRunes = 20
)

// NeedsVision reports whether f goes to the vision model: only photos, only
// when vision is enabled, and only when docling is not configured or its text
// has fewer than 20 non-space runes (FR-121). A file the vision model already
// read, or whose vision budget is spent, is not sent again.
func NeedsVision(f File, enabled, doclingConfigured bool) bool {
	if !enabled || f.Kind != FileImage || f.TextSource == TextVision {
		return false
	}
	if f.State == FileEmpty && f.VisionAttempts > 0 {
		return false
	}
	if !doclingConfigured {
		return true
	}

	return nonSpaceRunes(f.Text) < visionMinRunes
}

func nonSpaceRunes(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}

	return n
}

// VisionFailed records one failed vision attempt on f at now and returns the
// outcome for the step. A temporary or misconfig failure within the budget is
// returned as is, without a dependency to wait on, so it is retried. A
// permanent failure, or the third attempt, or two minutes since the first
// attempt, leaves f empty and returns OK: the hint falls back and extraction
// carries on (FR-123).
func VisionFailed(f *File, o Outcome, now time.Time) Outcome {
	f.VisionAttempts++
	if f.VisionStartedAt.IsZero() {
		f.VisionStartedAt = now
	}

	exhausted := f.VisionAttempts >= visionMaxAttempts ||
		now.Sub(f.VisionStartedAt) >= visionMaxDuration
	if o.Class == ClassPermanent || exhausted {
		f.State = FileEmpty
		return OK()
	}
	o.WaitingOn = ""

	return o
}
