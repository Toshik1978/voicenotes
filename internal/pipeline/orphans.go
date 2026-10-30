package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// OrphanAudioAge is how old an unreferenced audio file must be before the
// startup sweep deletes it. A file is legitimately unreferenced only for the
// moment between its write and the row commit (or the temp-file rename), so
// any age well above that is safe; an hour also tolerates clock adjustments
// and an old instance still draining during a redeploy on the same volume.
const OrphanAudioAge = time.Hour

// AudioFile is one directory entry of the audio dir.
type AudioFile struct {
	Name    string
	ModTime time.Time
	Regular bool
}

// AudioRefs lists the audio file names that capture rows reference.
type AudioRefs interface {
	AudioPaths(ctx context.Context) ([]string, error)
}

// OrphanAudio returns, in listing order, the regular files that no capture
// references and that were last modified more than OrphanAudioAge before now.
// They are what a crash leaves behind: a tg-* file written before its row was
// committed, or a .tmp-* file never renamed.
func OrphanAudio(files []AudioFile, referenced map[string]bool, now time.Time) []string {
	var out []string
	cutoff := now.Add(-OrphanAudioAge)
	for _, f := range files {
		if !f.Regular || referenced[f.Name] || !f.ModTime.Before(cutoff) {
			continue
		}
		out = append(out, f.Name)
	}

	return out
}

// SweepOrphanAudio deletes orphaned audio files in dir (see OrphanAudio) and
// returns how many it removed. It must run before anything writes audio. If
// the referenced names cannot be read it deletes nothing. A file that cannot
// be removed is counted in the log and skipped. Only counts are logged, never
// names or contents. A nil log means no logging.
func SweepOrphanAudio(ctx context.Context, dir string, refs AudioRefs, now time.Time, log *zap.Logger) (int, error) {
	if log == nil {
		log = zap.NewNop()
	}
	files, err := listAudio(dir)
	if err != nil || len(files) == 0 {
		return 0, err
	}
	referenced, err := referencedAudio(ctx, refs)
	if err != nil {
		return 0, err
	}

	removed, failed := 0, 0
	for _, name := range OrphanAudio(files, referenced, now) {
		err := os.Remove(filepath.Join(dir, name))
		switch {
		case err == nil:
			removed++
		case !errors.Is(err, fs.ErrNotExist): // already gone: nothing removed, nothing failed
			failed++
		}
	}
	if removed > 0 || failed > 0 {
		log.Info("swept orphan audio files", zap.Int("removed", removed), zap.Int("failed", failed))
	}

	return removed, nil
}

func referencedAudio(ctx context.Context, refs AudioRefs) (map[string]bool, error) {
	paths, err := refs.AudioPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("pipeline: list referenced audio: %w", err)
	}
	referenced := make(map[string]bool, len(paths))
	for _, p := range paths {
		referenced[p] = true
	}

	return referenced, nil
}

// listAudio lists dir; a missing dir holds no files.
func listAudio(dir string) ([]AudioFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pipeline: list audio dir: %w", err)
	}
	files := make([]AudioFile, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // vanished since the listing
		}
		files = append(files, AudioFile{Name: e.Name(), ModTime: info.ModTime(), Regular: info.Mode().IsRegular()})
	}

	return files, nil
}
