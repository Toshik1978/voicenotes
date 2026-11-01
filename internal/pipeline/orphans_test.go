package pipeline_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

func TestOrphanFiles(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-pipeline.OrphanFileAge - time.Second)
	files := []pipeline.StoredFile{
		{Name: "tg--1001234567890-5-123.ogg", ModTime: old, Regular: true},        // orphan
		{Name: ".tmp-456", ModTime: old, Regular: true},                           // orphan temp file
		{Name: "7.ogg", ModTime: old, Regular: true},                              // referenced
		{Name: "8.ogg", ModTime: now.Add(-time.Minute), Regular: true},            // too young
		{Name: "9.ogg", ModTime: now.Add(-pipeline.OrphanFileAge), Regular: true}, // exactly the age: kept
		{Name: "sub", ModTime: old, Regular: false},                               // not a regular file
		{Name: "10.ogg", ModTime: now.Add(time.Hour), Regular: true},              // clock skew: future
	}
	referenced := map[string]bool{"7.ogg": true}

	got := pipeline.OrphanFiles(files, referenced, now)
	want := []string{"tg--1001234567890-5-123.ogg", ".tmp-456"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OrphanFiles = %v, want %v", got, want)
	}
	if pipeline.OrphanFileAge != time.Hour {
		t.Errorf("OrphanFileAge = %v, want 1h", pipeline.OrphanFileAge)
	}
}

func TestOrphanFilesEmpty(t *testing.T) {
	if got := pipeline.OrphanFiles(nil, nil, time.Now()); len(got) != 0 {
		t.Fatalf("OrphanFiles(nil) = %v", got)
	}
}

type fakeRefs struct {
	paths []string
	err   error
}

func (f fakeRefs) FilePaths(context.Context) ([]string, error) { return f.paths, f.err }

func writeAged(t *testing.T, dir, name string, mod time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestSweepOrphanFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	writeAged(t, dir, "tg-1-2-3.ogg", old)
	writeAged(t, dir, ".tmp-4", old)
	writeAged(t, dir, "5.ogg", old)
	writeAged(t, dir, "6.ogg", now)
	if err := os.Mkdir(filepath.Join(dir, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}

	n, err := pipeline.SweepOrphanFiles(context.Background(), dir, fakeRefs{paths: []string{"5.ogg"}}, now, nil)
	if err != nil || n != 2 {
		t.Fatalf("SweepOrphanFiles = %d, %v; want 2, nil", n, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	left := make([]string, 0, len(entries))
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"5.ogg", "6.ogg", "keep"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}

func TestSweepOrphanFilesDeletesNothingWhenRefsFail(t *testing.T) {
	dir := t.TempDir()
	writeAged(t, dir, "tg-1-2-3.ogg", time.Now().Add(-2*time.Hour))

	refs := fakeRefs{err: errors.New("db locked")}
	n, err := pipeline.SweepOrphanFiles(context.Background(), dir, refs, time.Now(), nil)
	if err == nil || n != 0 {
		t.Fatalf("SweepOrphanFiles = %d, %v; want 0 and an error", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tg-1-2-3.ogg")); err != nil {
		t.Fatalf("file removed despite the error: %v", err)
	}
}

func TestSweepOrphanFilesMissingDir(t *testing.T) {
	n, err := pipeline.SweepOrphanFiles(
		context.Background(),
		filepath.Join(t.TempDir(), "none"),
		fakeRefs{},
		time.Now(),
		nil,
	)
	if err != nil || n != 0 {
		t.Fatalf("SweepOrphanFiles = %d, %v; want 0, nil", n, err)
	}
}

// vanishingRefs deletes the named file while the references are read, which
// is after the directory listing: the file then vanishes before its Remove.
type vanishingRefs struct{ path string }

func (v vanishingRefs) FilePaths(context.Context) ([]string, error) {
	return nil, os.Remove(v.path)
}

func TestSweepOrphanFilesDoesNotCountVanishedFile(t *testing.T) {
	dir := t.TempDir()
	writeAged(t, dir, "tg-1-2-3.ogg", time.Now().Add(-2*time.Hour))

	refs := vanishingRefs{path: filepath.Join(dir, "tg-1-2-3.ogg")}
	n, err := pipeline.SweepOrphanFiles(context.Background(), dir, refs, time.Now(), nil)
	if err != nil || n != 0 {
		t.Fatalf("SweepOrphanFiles = %d, %v; want 0, nil", n, err)
	}
}

func TestSweepOrphanFilesCountsFailedRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	writeAged(t, dir, "tg-1-2-3.ogg", time.Now().Add(-2*time.Hour))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	core, logs := observer.New(zap.InfoLevel)
	n, err := pipeline.SweepOrphanFiles(context.Background(), dir, fakeRefs{}, time.Now(), zap.New(core))
	if err != nil || n != 0 {
		t.Fatalf("SweepOrphanFiles = %d, %v; want 0, nil", n, err)
	}
	entries := logs.FilterMessage("swept orphan files").All()
	if len(entries) != 1 || entries[0].ContextMap()["failed"] != int64(1) {
		t.Fatalf("log = %v, want one entry with failed=1", entries)
	}
}
