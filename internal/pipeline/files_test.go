package pipeline_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

func TestDirFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "files")
	a := pipeline.NewDirFiles(dir)

	if ok, err := a.Exists("1.ogg"); err != nil || ok {
		t.Fatalf("exists before write: %v %v", ok, err)
	}
	if err := a.Write("1.ogg", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := a.Write("1.ogg", []byte("y")); err != nil {
		t.Fatal(err)
	}
	got, err := a.Read("1.ogg")
	if err != nil || string(got) != "y" {
		t.Fatalf("read %q %v", got, err)
	}
	if ok, _ := a.Exists("1.ogg"); !ok {
		t.Error("should exist")
	}
	if err := a.Remove("1.ogg"); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove("1.ogg"); err != nil {
		t.Errorf("remove missing: %v", err)
	}
	if _, err := a.Read("1.ogg"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read missing: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}
	for _, bad := range []string{"../x", "a/b", ""} {
		if err := a.Write(bad, nil); err == nil {
			t.Errorf("write %q should fail", bad)
		}
	}
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func TestMigrateFilesDirRenamesAudio(t *testing.T) {
	data := t.TempDir()
	writeTree(t, filepath.Join(data, "audio"), map[string]string{"a.ogg": "A"})
	if err := pipeline.MigrateFilesDir(data); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(data, "files", "a.ogg")); got != "A" {
		t.Fatalf("files/a.ogg = %q", got)
	}
	if _, err := os.Stat(filepath.Join(data, "audio")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("audio/ should be gone: %v", err)
	}
}

func TestMigrateFilesDirNoop(t *testing.T) {
	for name, setup := range map[string]func(string){
		"neither":    func(string) {},
		"files only": func(d string) { writeTree(t, filepath.Join(d, "files"), map[string]string{"f": "F"}) },
	} {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			setup(data)
			if err := pipeline.MigrateFilesDir(data); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(data, "audio")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("audio/ appeared: %v", err)
			}
		})
	}
}

func TestMigrateFilesDirMergesAndRemovesAudio(t *testing.T) {
	data := t.TempDir()
	writeTree(t, filepath.Join(data, "audio"), map[string]string{"a.ogg": "A", "b.ogg": "B"})
	writeTree(t, filepath.Join(data, "files"), map[string]string{"c.pdf": "C"})
	if err := pipeline.MigrateFilesDir(data); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]string{"a.ogg": "A", "b.ogg": "B", "c.pdf": "C"} {
		if got := readFile(t, filepath.Join(data, "files", n)); got != want {
			t.Errorf("files/%s = %q, want %q", n, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "audio")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("audio/ should be removed: %v", err)
	}
}

// A name present in both directories is never overwritten: the entry stays in
// audio/, which is kept, and both contents survive.
func TestMigrateFilesDirKeepsCollidingEntry(t *testing.T) {
	data := t.TempDir()
	writeTree(t, filepath.Join(data, "audio"), map[string]string{"same.ogg": "OLD", "a.ogg": "A"})
	writeTree(t, filepath.Join(data, "files"), map[string]string{"same.ogg": "NEW"})
	if err := pipeline.MigrateFilesDir(data); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(data, "files", "same.ogg")); got != "NEW" {
		t.Errorf("files/same.ogg = %q, want NEW", got)
	}
	if got := readFile(t, filepath.Join(data, "audio", "same.ogg")); got != "OLD" {
		t.Errorf("audio/same.ogg = %q, want OLD", got)
	}
	if got := readFile(t, filepath.Join(data, "files", "a.ogg")); got != "A" {
		t.Errorf("files/a.ogg = %q", got)
	}
	// Idempotent: a second run changes nothing and loses nothing.
	if err := pipeline.MigrateFilesDir(data); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(data, "audio", "same.ogg")); got != "OLD" {
		t.Errorf("after rerun audio/same.ogg = %q", got)
	}
}

func TestMigrateFilesDirErrors(t *testing.T) {
	data := t.TempDir()
	// audio is a regular file and files is absent: rename succeeds, so make
	// files unreadable-as-dir instead: both are regular files.
	writeTree(t, data, map[string]string{"audio": "x", "files": "y"})
	if err := pipeline.MigrateFilesDir(data); err == nil {
		t.Fatal("want an error when audio and files are not directories")
	}
}
