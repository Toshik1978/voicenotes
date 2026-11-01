package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	stdpath "path"
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

// filePattern is the os.CreateTemp pattern for a file item:
// tg-<chat>-<msg>-<random><ext>, the same scheme as audioPattern. The
// extension comes from the sanitised original name, so it is safe on disk.
func filePattern(m Message) string {
	ext := pipeline.SanitizeExt(stdpath.Ext(pipeline.SanitizeFileName(m.Files.Name)))

	return fmt.Sprintf("tg-%d-%d-*%s", m.ChatID, m.ID, ext)
}

// files stores one photo, document or video as a files capture. Like audio
// it downloads and fsyncs first, then commits the row and sends ⏳. If the
// file cannot be fetched or written, the item is still committed, with no
// local path and no reply, and the pipeline's download step fetches it
// later.
func (h *Handler) files(ctx context.Context, m Message) {
	if m.Files.Size > MaxFileSize {
		h.tooLarge(ctx, m)

		return
	}

	item := pipeline.FileItem{
		Source: Source, ChatID: m.ChatID, MessageID: m.ID, ReceivedAt: m.Date,
		Kind: m.Files.Kind, FileID: m.Files.FileID, MimeType: m.Files.MIME,
		Name: m.Files.Name, Size: m.Files.Size, Caption: m.Files.Caption, AlbumID: m.Files.AlbumID,
	}

	data, err := h.dl.Download(ctx, m.Files.FileID)
	if errors.Is(err, ErrTooLarge) {
		h.tooLarge(ctx, m)

		return
	}
	if err != nil {
		h.log.Warn("file download failed, queueing for the download step",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID), zap.Error(err))
		h.ingestFile(ctx, m, item, nil)

		return
	}
	name, err := writeAudio(h.filesDir, filePattern(m), data)
	if err != nil {
		h.log.Error("writing file failed, queueing for the download step",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID), zap.Error(err))
		h.ingestFile(ctx, m, item, nil)

		return
	}

	item.LocalPath = name
	if item.Size == 0 {
		item.Size = int64(len(data))
	}
	if !h.ingestFile(ctx, m, item, h.ack(m)) {
		// Duplicate or store failure: the file belongs to no row.
		removeAudio(h.filesDir, name, h.log)
	}
}

// ingestFile stores item and reports whether it was newly stored, with the
// same duplicate and failure handling as ingest.
func (h *Handler) ingestFile(ctx context.Context, m Message, item pipeline.FileItem, ack pipeline.Ack) bool {
	stored, err := h.ingester.IngestFile(ctx, item, ack)
	switch {
	case errors.Is(err, pipeline.ErrDuplicate):
		h.log.Info("duplicate delivery ignored", zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID))

		return false
	case err != nil:
		h.log.Error("storing file capture failed; asking the sender to resend",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID), zap.Error(err))
		h.reply(ctx, m, TextNotSaved)

		return false
	}
	h.log.Info("file stored",
		zap.Int64("capture", stored.ID), zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID),
		zap.Stringer("kind", item.Kind))

	return true
}
