package pipeline

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DirAudio is the AudioStore over a directory (DATA_DIR/audio).
type DirAudio struct{ dir string }

// NewDirAudio returns a store rooted at dir, created on first write.
func NewDirAudio(dir string) *DirAudio { return &DirAudio{dir: dir} }

// Write durably replaces name: temp file, fsync, rename, then fsync of the
// directory, so a crash leaves either the old file or the whole new one.
func (a *DirAudio) Write(name string, data []byte) error {
	p, err := a.path(name)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(a.dir, 0o750); err != nil {
		return fmt.Errorf("pipeline: create audio dir: %w", err)
	}
	f, err := os.CreateTemp(a.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("pipeline: create audio file: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("pipeline: write audio file: %w", err)
	}
	if err = os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("pipeline: write audio file: %w", err)
	}
	d, err := os.Open(a.dir)
	if err != nil {
		return fmt.Errorf("pipeline: sync audio dir: %w", err)
	}
	serr = d.Sync()

	return errors.Join(serr, d.Close())
}

// Read returns the file's bytes; a missing file wraps fs.ErrNotExist.
func (a *DirAudio) Read(name string) ([]byte, error) {
	p, err := a.path(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("pipeline: read audio: %w", err)
	}

	return data, nil
}

// Exists reports whether the file is present.
func (a *DirAudio) Exists(name string) (bool, error) {
	p, err := a.path(name)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pipeline: stat audio: %w", err)
	}

	return true, nil
}

// Remove deletes the file; a missing file is not an error.
func (a *DirAudio) Remove(name string) error {
	p, err := a.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pipeline: remove audio: %w", err)
	}

	return nil
}

func (a *DirAudio) path(name string) (string, error) {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("pipeline: invalid audio file name %q", name)
	}

	return filepath.Join(a.dir, name), nil
}
