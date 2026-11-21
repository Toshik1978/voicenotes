package pipeline_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

// FR-210b: Hold moves a file from DATA_DIR/files to DATA_DIR/nospeech under
// the given name, creating the directory; a missing file is not an error, so
// a retry after the move succeeds.
func TestDirFilesHold(t *testing.T) {
	data := t.TempDir()
	a := pipeline.NewDirFiles(filepath.Join(data, "files"))
	if err := a.Write("rec-1.wav", []byte("WAV")); err != nil {
		t.Fatal(err)
	}
	if err := a.Hold("rec-1.wav", "5.wav"); err != nil {
		t.Fatalf("hold: %v", err)
	}
	held := filepath.Join(data, pipeline.NoSpeechDir, "5.wav")
	if got, err := os.ReadFile(held); err != nil || string(got) != "WAV" {
		t.Fatalf("held file %q, %v", got, err)
	}
	if fi, err := os.Stat(filepath.Dir(held)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("hold dir %v, %v; want mode 0700", fi, err)
	}
	if ok, _ := a.Exists("rec-1.wav"); ok {
		t.Error("source still in files")
	}
	if err := a.Hold("rec-1.wav", "5.wav"); err != nil {
		t.Errorf("hold again: %v", err)
	}
	if got, err := os.ReadFile(held); err != nil || string(got) != "WAV" {
		t.Errorf("held file after retry %q, %v", got, err)
	}
	for _, bad := range [][2]string{{"../x", "5.wav"}, {"rec-1.wav", "../5.wav"}, {"", "5.wav"}, {"a", ""}} {
		if err := a.Hold(bad[0], bad[1]); err == nil {
			t.Errorf("hold %q -> %q: want an error", bad[0], bad[1])
		}
	}
}

// Review 1: a retry after the rename (crash or failed fsync before it was
// durable) still fsyncs both directories, and reports a failed fsync.
func TestDirFilesHoldRetrySyncsDirs(t *testing.T) {
	data := t.TempDir()
	filesDir, holdDir := filepath.Join(data, "files"), filepath.Join(data, pipeline.NoSpeechDir)
	a := pipeline.NewDirFiles(filesDir)
	if err := a.Write("rec-1.wav", []byte("WAV")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(holdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The rename happened, but its fsync never did.
	if err := os.Rename(filepath.Join(filesDir, "rec-1.wav"), filepath.Join(holdDir, "5.wav")); err != nil {
		t.Fatal(err)
	}
	var synced []string
	errSync := errors.New("EIO")
	var fail bool
	a.SetSyncDir(func(dir string) error {
		synced = append(synced, dir)
		if fail {
			return errSync
		}

		return nil
	})

	if err := a.Hold("rec-1.wav", "5.wav"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(synced) != 2 || !slices.Contains(synced, holdDir) || !slices.Contains(synced, filesDir) {
		t.Errorf("synced %q, want both %q and %q", synced, holdDir, filesDir)
	}
	fail = true
	if err := a.Hold("rec-1.wav", "5.wav"); !errors.Is(err, errSync) {
		t.Errorf("retry with a failing fsync = %v, want it reported", err)
	}
}

// Review 1 of T028: the hold starts when the audio is held. Hold stamps the
// held file's mtime with its clock, so a capture whose upload is older than
// MIC_NOSPEECH_HOLD (an STT outage) is not purged right after the move. The
// stamp is in place before the directory fsyncs.
func TestDirFilesHoldStampsHoldTime(t *testing.T) {
	heldAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	hold := 168 * time.Hour
	for _, retry := range []bool{false, true} {
		data := t.TempDir()
		filesDir, holdDir := filepath.Join(data, "files"), filepath.Join(data, pipeline.NoSpeechDir)
		a := pipeline.NewDirFiles(filesDir)
		a.SetNow(func() time.Time { return heldAt })
		if err := a.Write("rec-1.wav", []byte("WAV")); err != nil {
			t.Fatal(err)
		}
		uploaded := heldAt.Add(-2 * hold)
		if err := os.Chtimes(filepath.Join(filesDir, "rec-1.wav"), uploaded, uploaded); err != nil {
			t.Fatal(err)
		}
		held := filepath.Join(holdDir, "5.wav")
		if retry { // a crash between the rename and the stamp
			if err := os.MkdirAll(holdDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(filesDir, "rec-1.wav"), held); err != nil {
				t.Fatal(err)
			}
		}
		var stampedBeforeSync bool
		a.SetSyncDir(func(string) error {
			fi, err := os.Stat(held)
			stampedBeforeSync = err == nil && fi.ModTime().Equal(heldAt)

			return nil
		})

		if err := a.Hold("rec-1.wav", "5.wav"); err != nil {
			t.Fatalf("retry=%v: hold: %v", retry, err)
		}
		if fi, err := os.Stat(held); err != nil || !fi.ModTime().Equal(heldAt) {
			t.Fatalf("retry=%v: held mtime %v, %v; want %v", retry, fi, err, heldAt)
		}
		if !stampedBeforeSync {
			t.Errorf("retry=%v: mtime not stamped before the directory fsync", retry)
		}
		purge := pipeline.HeldAudio{Dir: holdDir, Hold: hold}
		n, err := purge.PurgeHeld(context.Background(), heldAt.Add(hold-time.Minute))
		if err != nil || n != 0 || !exists(t, held) {
			t.Errorf("retry=%v: purge inside the hold removed %d, %v", retry, n, err)
		}
	}
}
