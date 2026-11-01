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

// OrphanFileAge is how old an unreferenced file must be before the
// startup sweep deletes it. A file is legitimately unreferenced only for the
// moment between its write and the row commit (or the temp-file rename), so
// any age well above that is safe; an hour also tolerates clock adjustments
// and an old instance still draining during a redeploy on the same volume.
const OrphanFileAge = time.Hour

// StoredFile is one directory entry of the files dir.
type StoredFile struct {
	Name    string
	ModTime time.Time
	Regular bool
}

// FileRefs lists the file names that capture rows reference.
type FileRefs interface {
	FilePaths(ctx context.Context) ([]string, error)
}

// OrphanFiles returns, in listing order, the regular files that no capture
// references and that were last modified more than OrphanFileAge before now.
// They are what a crash leaves behind: a tg-* file written before its row was
// committed, or a .tmp-* file never renamed.
func OrphanFiles(files []StoredFile, referenced map[string]bool, now time.Time) []string {
	var out []string
	cutoff := now.Add(-OrphanFileAge)
	for _, f := range files {
		if !f.Regular || referenced[f.Name] || !f.ModTime.Before(cutoff) {
			continue
		}
		out = append(out, f.Name)
	}

	return out
}

// SweepOrphanFiles deletes orphaned files in dir (see OrphanFiles) and
// returns how many it removed. It must run before anything writes files. If
// the referenced names cannot be read it deletes nothing. A file that cannot
// be removed is counted in the log and skipped. Only counts are logged, never
// names or contents. A nil log means no logging.
func SweepOrphanFiles(ctx context.Context, dir string, refs FileRefs, now time.Time, log *zap.Logger) (int, error) {
	if log == nil {
		log = zap.NewNop()
	}
	files, err := listFiles(dir)
	if err != nil || len(files) == 0 {
		return 0, err
	}
	referenced, err := referencedFiles(ctx, refs)
	if err != nil {
		return 0, err
	}

	removed, failed := 0, 0
	for _, name := range OrphanFiles(files, referenced, now) {
		err := os.Remove(filepath.Join(dir, name))
		switch {
		case err == nil:
			removed++
		case !errors.Is(err, fs.ErrNotExist): // already gone: nothing removed, nothing failed
			failed++
		}
	}
	if removed > 0 || failed > 0 {
		log.Info("swept orphan files", zap.Int("removed", removed), zap.Int("failed", failed))
	}

	return removed, nil
}

func referencedFiles(ctx context.Context, refs FileRefs) (map[string]bool, error) {
	paths, err := refs.FilePaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("pipeline: list referenced files: %w", err)
	}
	referenced := make(map[string]bool, len(paths))
	for _, p := range paths {
		referenced[p] = true
	}

	return referenced, nil
}

// listFiles lists dir; a missing dir holds no files.
func listFiles(dir string) ([]StoredFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pipeline: list files dir: %w", err)
	}
	files := make([]StoredFile, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // vanished since the listing
		}
		files = append(files, StoredFile{Name: e.Name(), ModTime: info.ModTime(), Regular: info.Mode().IsRegular()})
	}

	return files, nil
}
