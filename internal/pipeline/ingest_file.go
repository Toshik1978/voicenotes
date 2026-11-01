package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.uber.org/zap"

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

// ArrivalDownloadLimit bounds the download of a batch's first item on
// arrival. A slower one is left to the download step, bounded only by the
// source's download client, so the chat's next items and a shutdown never
// wait on it for long.
const ArrivalDownloadLimit = time.Minute

// IngestFile durably stores item as part of a files capture: the store joins
// it to the chat's open batch or starts one, and owns the batch deadline.
// Only a created capture is acknowledged, and only once its item is held
// locally (Constitution VI, FR-103): the item is downloaded, written and
// fsynced, and its local path recorded, before ack runs. When that fails
// the item stays stored without data and ack does not run; the download
// step fetches it and sends the ⏳ then. Either way the batch's quiet period
// then restarts (the download and the ack may have outlasted it), and the
// worker is woken only when a capture was created or its deadline moved.
// A joining item is stored row-first and left to the download step: the
// batch's one ⏳ has answered its first item already. It mirrors Ingest: a
// redelivered item returns ErrDuplicate with no ack and no wake. With a nil
// ack nothing is fetched on arrival. WithFiles must have configured the
// Ingester.
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
	stored, err := i.storeAndAck(ctx, c, i.ackWhenHeld(ack), func() (capture.Capture, bool, error) {
		res, appendErr := i.appendFile(ctx, c, joinOnly)
		if appendErr != nil {
			return capture.Capture{}, false, appendErr
		}
		wake = res.Created || res.DeadlineMoved

		return res.Capture, res.Created, nil
	}, i.restartQuiet)
	if err != nil {
		return capture.Capture{}, err
	}
	if wake {
		i.waker.Wake()
	}

	return stored, nil
}

// ackWhenHeld wraps ack so that it runs only once the capture's file is held
// locally (keepFirstFile). A nil ack stays nil.
func (i *Ingester) ackWhenHeld(ack Ack) Ack {
	if ack == nil {
		return nil
	}

	return func(ctx context.Context, created capture.Capture) {
		if held, ok := i.keepFirstFile(ctx, created); ok {
			ack(ctx, held)
		}
	}
}

// keepFirstFile makes sure the one file of stored, a capture just created,
// is held locally, and reports whether it is: a file already held, or one
// that failed at arrival and so has nothing to hold, counts as held. A file
// it cannot hold is left for the download step; nothing written for it is
// kept.
func (i *Ingester) keepFirstFile(ctx context.Context, stored capture.Capture) (capture.Capture, bool) {
	f := stored.Files[0]
	if f.State != capture.FilePending || f.LocalPath != "" {
		return stored, true
	}
	log := i.log.With(zap.Int64("capture", stored.ID))
	dctx, cancel := context.WithTimeout(ctx, ArrivalDownloadLimit)
	data, err := i.fetch.Download(dctx, f.FileID)
	cancel()
	if err != nil {
		log.Warn("downloading the first file failed, leaving it to the download step", zap.Error(err))

		return stored, false
	}
	name := localName(stored.ID, f)
	if err = i.held.Write(name, data); err != nil {
		log.Error("writing the first file failed, leaving it to the download step", zap.Error(err))

		return stored, false
	}
	if err = i.files.SetFileLocalPath(ctx, stored.ID, f.Position, name); err != nil {
		log.Error("recording the first file failed, leaving it to the download step", zap.Error(err))
		if rerr := i.held.Remove(name); rerr != nil {
			log.Warn("removing the unrecorded first file failed", zap.Error(rerr))
		}

		return stored, false
	}
	stored.Files = slices.Clone(stored.Files)
	stored.Files[0].LocalPath = name

	return stored, true
}

// restartQuiet restarts the quiet period of a batch the ack just answered,
// while the worker is still held off it. The ⏳ send can outlast the quiet
// period, and the chat's next items wait behind it (the source hands one
// chat's items over one at a time), so without this the worker could close
// the batch before the next album part joins (T030). A failure only leaves
// the deadline set at the append, so it is logged, not returned.
func (i *Ingester) restartQuiet(ctx context.Context, stored capture.Capture) {
	if err := i.files.RestartQuiet(ctx, stored.ID, i.quiet); err != nil {
		i.log.Warn("restarting the batch quiet period failed", zap.Int64("capture", stored.ID), zap.Error(err))
	}
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
