package capture

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
	// Attached is true once attached to the note (or the attach failed for good).
	Attached      bool
	AttachFailure string
}
