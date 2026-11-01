package pipeline

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DirFiles is the FileStore over a directory (DATA_DIR/files).
type DirFiles struct{ dir string }

// NewDirFiles returns a store rooted at dir, created on first write.
func NewDirFiles(dir string) *DirFiles { return &DirFiles{dir: dir} }

// Write durably replaces name: temp file, fsync, rename, then fsync of the
// directory, so a crash leaves either the old file or the whole new one.
func (a *DirFiles) Write(name string, data []byte) error {
	p, err := a.path(name)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(a.dir, 0o750); err != nil {
		return fmt.Errorf("pipeline: create files dir: %w", err)
	}
	f, err := os.CreateTemp(a.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("pipeline: create file: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("pipeline: write file: %w", err)
	}
	if err = os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("pipeline: write file: %w", err)
	}
	d, err := os.Open(a.dir)
	if err != nil {
		return fmt.Errorf("pipeline: sync files dir: %w", err)
	}
	serr = d.Sync()

	return errors.Join(serr, d.Close())
}

// Read returns the file's bytes; a missing file wraps fs.ErrNotExist.
func (a *DirFiles) Read(name string) ([]byte, error) {
	p, err := a.path(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("pipeline: read file: %w", err)
	}

	return data, nil
}

// Exists reports whether the file is present.
func (a *DirFiles) Exists(name string) (bool, error) {
	p, err := a.path(name)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pipeline: stat file: %w", err)
	}

	return true, nil
}

// Remove deletes the file; a missing file is not an error.
func (a *DirFiles) Remove(name string) error {
	p, err := a.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pipeline: remove file: %w", err)
	}

	return nil
}

func (a *DirFiles) path(name string) (string, error) {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("pipeline: invalid file name %q", name)
	}

	return filepath.Join(a.dir, name), nil
}

// MigrateFilesDir renames the legacy DATA_DIR/audio to DATA_DIR/files. Run it
// once at startup, before the workers. With only audio/ present it renames the
// directory (stored names are relative, so rows stay valid). With both present
// it moves the entries of audio/ into files/ and removes audio/ once empty. An
// entry whose name already exists in files/ is never overwritten: it stays in
// audio/, and audio/ is kept, so no file is lost. With no audio/ it does
// nothing, which also makes a repeated run harmless.
func MigrateFilesDir(dataDir string) error {
	oldDir := filepath.Join(dataDir, "audio")
	newDir := filepath.Join(dataDir, "files")

	if _, err := os.Lstat(oldDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("pipeline: stat legacy audio dir: %w", err)
	}
	if _, err := os.Lstat(newDir); errors.Is(err, fs.ErrNotExist) {
		if err = os.Rename(oldDir, newDir); err != nil {
			return fmt.Errorf("pipeline: rename audio dir to files: %w", err)
		}

		return nil
	} else if err != nil {
		return fmt.Errorf("pipeline: stat files dir: %w", err)
	}

	return mergeLegacyDir(oldDir, newDir)
}

// mergeLegacyDir moves the entries of oldDir into newDir, skipping names that
// already exist there, and removes oldDir only when nothing was skipped.
func mergeLegacyDir(oldDir, newDir string) error {
	entries, err := os.ReadDir(oldDir)
	if err != nil {
		return fmt.Errorf("pipeline: list legacy audio dir: %w", err)
	}
	kept := 0
	for _, e := range entries {
		dst := filepath.Join(newDir, e.Name())
		_, err = os.Lstat(dst)
		switch {
		case err == nil:
			kept++ // collision: leave it in audio/
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("pipeline: stat files entry: %w", err)
		default:
			if err = os.Rename(filepath.Join(oldDir, e.Name()), dst); err != nil {
				return fmt.Errorf("pipeline: move audio entry to files: %w", err)
			}
		}
	}
	if kept > 0 {
		return nil
	}
	if err = os.Remove(oldDir); err != nil {
		return fmt.Errorf("pipeline: remove legacy audio dir: %w", err)
	}

	return nil
}
