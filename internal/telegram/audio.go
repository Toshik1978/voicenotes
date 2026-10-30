package telegram

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
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
		return fmt.Errorf("telegram: open audio dir: %w", err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if err := errors.Join(serr, cerr); err != nil {
		return fmt.Errorf("telegram: sync audio dir: %w", err)
	}

	return nil
}

// removeAudio deletes a file written for a capture that was not stored.
func removeAudio(dir, name string, log *zap.Logger) {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("removing unused audio file failed", zap.String("file", name), zap.Error(err))
	}
}
