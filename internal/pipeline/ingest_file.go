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
	// TooLarge is set for a file over the source's download limit. It is
	// kept as a failed file in its batch; one that would start a batch
	// alone (no album, no open batch) is rejected with ErrFileTooLarge.
	TooLarge bool
}

// IngestFile durably stores item as part of a files capture: the store joins
// it to the chat's open batch or starts one, and owns the batch deadline.
// It runs ack only when a capture was created, and wakes the worker only
// when a capture was created or its deadline moved. It mirrors Ingest: a
// redelivered item returns ErrDuplicate with no ack and no wake. WithFiles
// must have configured the Ingester.
//
// A TooLarge item is stored as a failed file. With an album ID it is
// appended like any item, since the rest of its album will follow; without
// one it only joins the chat's open batch, and when there is none nothing is
// stored and ErrFileTooLarge is returned, so the source can reject it.
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
	c.Files = []capture.File{{
		Source: item.Source, SourceChatID: item.ChatID, SourceMessageID: item.MessageID,
		Kind: item.Kind, FileID: item.FileID, MimeType: item.MimeType,
		FileName: item.Name, FileSize: item.Size, LocalPath: item.LocalPath,
		State: capture.FilePending,
	}}
	if item.TooLarge {
		c.Files[0].State, c.Files[0].Failure = capture.FileFailed, reasonTooLarge
	}
	joinOnly := item.TooLarge && item.AlbumID == ""

	var wake bool
	stored, err := i.storeAndAck(ctx, c, ack, func() (capture.Capture, bool, error) {
		res, appendErr := i.appendFile(ctx, c, joinOnly)
		if appendErr != nil {
			return capture.Capture{}, false, appendErr
		}
		wake = res.Created || res.DeadlineMoved

		return res.Capture, res.Created, nil
	})
	if err != nil {
		return capture.Capture{}, err
	}
	if wake {
		i.waker.Wake()
	}

	return stored, nil
}

// appendFile stores c through the FileAppender; with joinOnly it uses
// JoinOpenBatch and maps "not joined" to ErrFileTooLarge.
func (i *Ingester) appendFile(ctx context.Context, c capture.Capture, joinOnly bool) (capture.AppendResult, error) {
	if !joinOnly {
		res, err := i.files.AppendFile(ctx, c, i.quiet)
		if err != nil {
			return capture.AppendResult{}, fmt.Errorf("append file: %w", err)
		}

		return res, nil
	}
	res, joined, err := i.files.JoinOpenBatch(ctx, c, i.quiet)
	switch {
	case err != nil:
		return capture.AppendResult{}, fmt.Errorf("join open batch: %w", err)
	case !joined:
		// Nothing was stored: the item would have started a batch alone.
		return capture.AppendResult{}, ErrFileTooLarge
	}

	return res, nil
}
