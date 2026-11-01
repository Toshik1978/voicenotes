package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// audioPattern is the os.CreateTemp pattern for a message's audio file:
// tg-<chat>-<msg>-<random><ext>. The capture ID only exists after insert, so
// the name derives from the source identity (bead ruling); the random part
// keeps a redelivery from ever overwriting, and then removing, the file of
// the capture that was stored first.
func audioPattern(m Message) string {
	return fmt.Sprintf("tg-%d-%d-*%s", m.ChatID, m.ID, audioExt(m.Audio))
}

// audioExt picks the file extension from the MIME type; voice notes are
// always Ogg/Opus.
func audioExt(a *Audio) string {
	if a.Voice {
		return ".ogg"
	}
	switch a.MimeType {
	case "audio/ogg", "audio/opus", "":
		return ".ogg"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/m4a", "audio/x-m4a", "audio/aac":
		return ".m4a"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/flac", "audio/x-flac":
		return ".flac"
	case "audio/webm":
		return ".webm"
	}

	return ".bin"
}

// writeAudio durably writes data to a new file in dir and returns its name
// relative to dir. The file and the directory are fsynced, so the file
// survives a crash once writeAudio returns (Constitution VI). On any error
// nothing is left behind.
func writeAudio(dir, pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("telegram: create audio file: %w", err)
	}
	path := f.Name()
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(path)

		return "", fmt.Errorf("telegram: write audio file: %w", err)
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(path)

		return "", err
	}

	return filepath.Base(path), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("telegram: open files dir: %w", err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if err := errors.Join(serr, cerr); err != nil {
		return fmt.Errorf("telegram: sync files dir: %w", err)
	}

	return nil
}

// removeAudio deletes a file written for a capture that was not stored.
func removeAudio(dir, name string, log *zap.Logger) {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("removing unused audio file failed", zap.String("file", name), zap.Error(err))
	}
}

// files stores one photo, document or video as part of a files capture
// (a batch) the moment it arrives. The item's row commits first, so its
// batch position, caption order and quiet period are fixed in arrival
// order (FR-106, FR-107): Run hands one chat's file messages over one at a
// time, in delivery order, so a slow download can no longer reorder an
// album or push a part out of its batch, and the item that creates a batch
// really is its first. That item alone is then downloaded and fsynced here,
// and only then gets the batch's one ⏳ (Constitution VI, FR-103; the
// ingester holds the worker off it meanwhile and restarts the quiet period
// afterwards), so the download step finds it on disk and the reply
// recorded. If it cannot be downloaded or written it stays stored without
// data and no ⏳ is sent here: the download step fetches it and sends the
// ⏳ then. A joining item is not downloaded here; the pipeline's download
// step fetches it by its Telegram file ID once the batch closes, and the
// final reply confirms it.
//
// A file over the download limit (research R11) with an album ID, or one
// that joins the chat's open batch, is stored in the batch as a failed
// file, so the note keeps the rest and the reply names it (FR-126). A
// standalone one is stored nowhere and gets TextTooLarge, as for audio.
func (h *Handler) files(ctx context.Context, m Message) {
	item := pipeline.FileItem{
		Source: Source, ChatID: m.ChatID, MessageID: m.ID, ReceivedAt: m.Date,
		Kind: m.Files.Kind, FileID: m.Files.FileID, MimeType: m.Files.MIME,
		Name: m.Files.Name, Size: m.Files.Size, Caption: m.Files.Caption, AlbumID: m.Files.AlbumID,
		TooLarge: m.Files.Size > MaxFileSize,
	}
	stored, err := h.ingester.IngestFile(ctx, item, h.ack(m))
	switch {
	case errors.Is(err, pipeline.ErrFileTooLarge):
		h.tooLarge(ctx, m)
	case errors.Is(err, pipeline.ErrDuplicate):
		h.log.Info("duplicate delivery ignored", zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID))
	case err != nil:
		h.log.Error("storing file capture failed; asking the sender to resend",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID), zap.Error(err))
		h.reply(ctx, m, TextNotSaved)
	default:
		h.log.Info("file stored",
			zap.Int64("capture", stored.ID), zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID),
			zap.Stringer("kind", item.Kind))
	}
}
