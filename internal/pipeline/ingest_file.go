package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// FileItem is one incoming photo, document or video, as a source adapter
// hands it to IngestFile.
type FileItem struct {
	// Source, ChatID and MessageID are the item's identity (dedupe).
	Source    string
	ChatID    int64
	MessageID int64
	// ReceivedAt is when the item arrived.
	ReceivedAt time.Time

	Kind     capture.FileKind
	FileID   string
	MimeType string
	// Name is the original file name, if any.
	Name string
	Size int64
	// LocalPath is relative to the files directory; empty when not yet downloaded.
	LocalPath string
	// Caption is the user's text sent with the item.
	Caption string
	// AlbumID is the Telegram media group, if any.
	AlbumID string
}

// IngestFile durably stores item as part of a files capture, runs ack when
// that created a capture, and wakes the worker. It mirrors Ingest: a
// redelivered item returns ErrDuplicate with no ack and no wake. WithFiles
// must have configured the Ingester.
func (i *Ingester) IngestFile(ctx context.Context, item FileItem, ack Ack) (capture.Capture, error) {
	if i.files == nil {
		return capture.Capture{}, errors.New("pipeline: file ingest not configured")
	}
	c, err := capture.New(capture.KindFiles, capture.StepCollect, item.ReceivedAt)
	if err != nil {
		return capture.Capture{}, fmt.Errorf("pipeline: ingest file: %w", err)
	}
	c.Source, c.SourceChatID, c.SourceMessageID = item.Source, item.ChatID, item.MessageID
	c.AlbumID, c.Text = item.AlbumID, item.Caption
	c.NextAttemptAt = item.ReceivedAt.Add(i.quiet)
	c.Files = []capture.File{{
		Source: item.Source, SourceChatID: item.ChatID, SourceMessageID: item.MessageID,
		Kind: item.Kind, FileID: item.FileID, MimeType: item.MimeType,
		FileName: item.Name, FileSize: item.Size, LocalPath: item.LocalPath,
		State: capture.FilePending,
	}}

	stored, err := i.storeAndAck(ctx, c, ack, func() (capture.Capture, bool, error) {
		return i.files.AppendFile(ctx, c, i.quiet)
	})
	if err != nil {
		return capture.Capture{}, err
	}
	i.waker.Wake()

	return stored, nil
}
