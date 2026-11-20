package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// RecordingInserter is the slice of the capture queue a microphone recording
// needs. InsertRecording stores c like Inserter.Insert and, in the same
// transaction, takes the start reply recorded for c.SourceMessageID, if any,
// into c.ReplyMessageID. Duplicates are signalled like Inserter's.
type RecordingInserter interface {
	InsertRecording(ctx context.Context, c *capture.Capture) error
}

// WithRecordings enables IngestRecording: recordings are inserted through
// store and filed under notifyChatID, the chat that receives microphone
// notices.
func WithRecordings(store RecordingInserter, notifyChatID int64) IngesterOption {
	return func(i *Ingester) { i.recordings, i.notifyChatID = store, notifyChatID }
}

// RecordingMeta describes one finished microphone recording.
type RecordingMeta struct {
	// ID is the recorder-chosen recording ID, the capture's source message ID.
	ID int64
	// StartedAt is when the recording began; it becomes the note's time.
	StartedAt time.Time
	// EndReason and Recorder are informational: why it ended and which
	// recorder made it.
	EndReason string
	Recorder  string
}

// RecordingResult is the outcome of IngestRecording.
type RecordingResult int

// Recording outcomes.
const (
	// Stored means the recording was queued; the file now belongs to its row.
	Stored RecordingResult = iota + 1
	// Duplicate means the recording ID is already stored; the caller removes
	// its new file.
	Duplicate
)

// ErrNoRecordings is returned by IngestRecording on an Ingester built
// without WithRecordings.
var ErrNoRecordings = errors.New("pipeline: recordings not enabled")

// IngestRecording queues the WAV at localPath (relative to the files
// directory, already fsynced) as an audio capture starting at extract, timed
// at meta.StartedAt but due at the service clock. It inserts through
// InsertRecording, which attaches the start reply, with no ack, then wakes
// the worker.
func (i *Ingester) IngestRecording(
	ctx context.Context, meta RecordingMeta, localPath string,
) (RecordingResult, error) {
	if i.recordings == nil {
		return 0, ErrNoRecordings
	}
	c, err := capture.New(capture.KindAudio, capture.StepExtract, meta.StartedAt)
	if err != nil {
		return 0, fmt.Errorf("pipeline: recording capture: %w", err)
	}
	// The recorder's clock may run ahead: the note keeps StartedAt, but the
	// capture is due now on the service clock.
	c.NextAttemptAt, c.UpdatedAt = i.now(), i.now()
	c.Source, c.SourceChatID, c.SourceMessageID = capture.SourceMic, i.notifyChatID, meta.ID
	c.Files = []capture.File{{
		Source: c.Source, SourceChatID: c.SourceChatID, SourceMessageID: c.SourceMessageID,
		Kind: capture.FileAudio, State: capture.FilePending,
		MimeType: mimeWAV, LocalPath: localPath,
	}}

	switch _, err := i.ingest(ctx, c, nil, i.recordings.InsertRecording); {
	case errors.Is(err, ErrDuplicate):
		return Duplicate, nil
	case err != nil:
		return 0, err
	}

	return Stored, nil
}
