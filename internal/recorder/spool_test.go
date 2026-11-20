package recorder

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/mic"
)

var t0 = time.Date(2026, 11, 20, 9, 30, 0, 123456789, time.UTC) //nolint:gochecknoglobals // test fixture

// fakeClock is a hand-written clock the tests advance by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newSpool(t *testing.T) (*DirSpool, string, *fakeClock) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spool")
	clk := &fakeClock{t: t0}
	s, err := NewDirSpool(dir, clk.now)
	if err != nil {
		t.Fatalf("NewDirSpool: %v", err)
	}

	return s, dir, clk
}

func rec(id int64, start time.Time) Recording {
	return Recording{ID: id, Recorder: "kitchen", StartedAt: start}
}

func readSidecar(t *testing.T, dir string, id int64) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, idName(id)+".json"))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("sidecar json: %v", err)
	}

	return m
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func record(t *testing.T, s *DirSpool, r Recording, frames []byte) {
	t.Helper()
	w, err := s.Begin(r)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := w.Write(frames); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewDirSpoolCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if _, err := NewDirSpool(dir, nil); err != nil {
		t.Fatalf("NewDirSpool: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("probe left behind: %v", entries)
	}
}

func TestNewDirSpoolNotWritable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirSpool(filepath.Join(file, "spool"), nil); err == nil {
		t.Error("spool under a regular file: want error")
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirSpool(ro, nil); err == nil {
		t.Error("read-only spool: want error")
	}
}

func TestBeginWritesSidecarAndPlaceholder(t *testing.T) {
	s, dir, _ := newSpool(t)
	w, err := s.Begin(Recording{ID: 42, Recorder: "kitchen", StartedAt: t0.In(time.FixedZone("x", 3600))})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer w.Close()

	m := readSidecar(t, dir, 42)
	if m["id"] != float64(42) || m["recorder"] != "kitchen" || m["started_at"] != "2026-11-20T09:30:00.123456789Z" {
		t.Errorf("sidecar = %v", m)
	}
	if _, ok := m["ended_at"]; ok {
		t.Errorf("unfinished sidecar has ended_at: %v", m)
	}
	if _, ok := m["end_reason"]; ok {
		t.Errorf("unfinished sidecar has end_reason: %v", m)
	}
	b, err := os.ReadFile(filepath.Join(dir, "42.part.wav"))
	if err != nil {
		t.Fatalf("part file: %v", err)
	}
	if !bytes.Equal(b, mic.Header(0)) {
		t.Errorf("part file = %x, want placeholder header", b)
	}

	if _, err := s.Begin(rec(42, t0)); err == nil {
		t.Error("Begin with a used ID: want error")
	}
	if _, err := s.Begin(rec(0, t0)); err == nil {
		t.Error("Begin with ID 0: want error")
	}
}

func TestWriterSyncsAtMostEveryTwoSeconds(t *testing.T) {
	s, _, clk := newSpool(t)
	wc, err := s.Begin(rec(7, t0))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	w, ok := wc.(*partWriter)
	if !ok {
		t.Fatalf("writer type %T", wc)
	}
	syncs := 0
	w.sync = func() error { syncs++; return nil }

	steps := []struct {
		advance time.Duration
		want    int
	}{
		{0, 0},
		{time.Second, 0},
		{999 * time.Millisecond, 0},
		{time.Millisecond, 1}, // 2 s since Begin
		{time.Second, 1},
		{time.Second, 2},
		{10 * time.Second, 3},
	}
	for i, st := range steps {
		clk.t = clk.t.Add(st.advance)
		if n, err := w.Write([]byte{1, 2}); err != nil || n != 2 {
			t.Fatalf("step %d: Write = %d, %v", i, n, err)
		}
		if syncs != st.want {
			t.Errorf("step %d: syncs = %d, want %d", i, syncs, st.want)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if syncs != 4 {
		t.Errorf("Close did not sync: syncs = %d", syncs)
	}

	w2, _ := s.Begin(rec(8, t0))
	pw, _ := w2.(*partWriter)
	pw.sync = func() error { return errors.New("disk gone") }
	clk.t = clk.t.Add(time.Minute)
	if _, err := pw.Write([]byte{1, 2}); err == nil {
		t.Error("Write with failing sync: want error")
	}
	if err := pw.Close(); err == nil {
		t.Error("Close with failing sync: want error")
	}
}

func TestFinish(t *testing.T) {
	s, dir, _ := newSpool(t)
	record(t, s, rec(5, t0), make([]byte, 64001)) // trailing partial frame

	end := t0.Add(2 * time.Second)
	if err := s.Finish(5, end, mic.EndTap); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if exists(dir, "5.part.wav") {
		t.Error("part file still present")
	}
	f, err := os.Open(filepath.Join(dir, "5.wav"))
	if err != nil {
		t.Fatalf("open wav: %v", err)
	}
	defer f.Close()
	info, err := mic.Parse(f)
	if err != nil || info.DataBytes != 64000 {
		t.Errorf("Parse = %+v, %v; want 64000 data bytes", info, err)
	}
	if fi, _ := f.Stat(); fi.Size() != mic.HeaderSize+64000 {
		t.Errorf("size = %d, want truncated to whole frames", fi.Size())
	}
	m := readSidecar(t, dir, 5)
	if m["end_reason"] != "tap" || m["ended_at"] != "2026-11-20T09:30:02.123456789Z" {
		t.Errorf("sidecar = %v", m)
	}

	// Idempotent: a repeat after the rename only rewrites the sidecar.
	if err := s.Finish(5, end, mic.EndTap); err != nil {
		t.Errorf("repeat Finish: %v", err)
	}
	if err := s.Finish(99, end, mic.EndTap); err == nil {
		t.Error("Finish of an unknown ID: want error")
	}
}

func TestDiscard(t *testing.T) {
	s, dir, _ := newSpool(t)
	record(t, s, rec(3, t0), make([]byte, 10))
	if err := s.Discard(3); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	if err := s.Discard(3); err != nil {
		t.Errorf("repeat Discard: %v", err)
	}
}

func TestFinishedOldestFirst(t *testing.T) {
	s, _, _ := newSpool(t)
	for _, r := range []Recording{rec(30, t0.Add(time.Hour)), rec(10, t0), rec(20, t0.Add(time.Minute))} {
		record(t, s, r, make([]byte, 32))
		if err := s.Finish(r.ID, r.StartedAt.Add(time.Second), mic.EndMaxLength); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	}
	w, err := s.Begin(rec(40, t0.Add(-time.Hour))) // still recording: not listed
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	got, err := s.Finished()
	if err != nil {
		t.Fatalf("Finished: %v", err)
	}
	ids := make([]int64, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []int64{10, 20, 30}) {
		t.Fatalf("order = %v", ids)
	}
	first := got[0]
	if first.Recorder != "kitchen" || !first.StartedAt.Equal(t0) || !first.EndedAt.Equal(t0.Add(time.Second)) ||
		first.EndReason != mic.EndMaxLength {
		t.Errorf("first = %+v", first)
	}
}

func TestFinishedUnreadableSidecar(t *testing.T) {
	s, dir, _ := newSpool(t)
	record(t, s, rec(1, t0), make([]byte, 32))
	if err := s.Finish(1, t0, mic.EndTap); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finished(); err == nil {
		t.Error("corrupt sidecar: want error")
	}
}

func TestOpenAndRemove(t *testing.T) {
	s, dir, _ := newSpool(t)
	frames := []byte{1, 2, 3, 4}
	record(t, s, rec(9, t0), frames)
	if err := s.Finish(9, t0.Add(time.Second), mic.EndTap); err != nil {
		t.Fatal(err)
	}

	rc, size, err := s.Open(9)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(b)) || !bytes.Equal(b, append(mic.Header(4), frames...)) {
		t.Errorf("size %d, body %x", size, b)
	}
	if _, err := rc.Seek(0, io.SeekStart); err != nil {
		t.Errorf("Seek: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove(9); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if exists(dir, "9.wav") || exists(dir, "9.json") {
		t.Error("files remain after Remove")
	}
	if err := s.Remove(9); err != nil {
		t.Errorf("repeat Remove: %v", err)
	}
	if _, _, err := s.Open(9); err == nil {
		t.Error("Open of a removed recording: want error")
	}
}

func TestFreeBytes(t *testing.T) {
	s, _, _ := newSpool(t)
	n, err := s.FreeBytes()
	if err != nil || n <= 0 {
		t.Errorf("FreeBytes = %d, %v", n, err)
	}
	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreeBytes(); err == nil {
		t.Error("missing dir: want error")
	}
}

// crash leaves the files a power cut mid-recording would: a sidecar and a
// .part.wav whose header is still the placeholder.
func crash(t *testing.T, s *DirSpool, r Recording, frames int) {
	t.Helper()
	record(t, s, r, make([]byte, frames))
}

func write(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	s, dir, _ := newSpool(t)
	crash(t, s, rec(11, t0), 3*32000+1) // 3 s plus a partial frame

	got, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !slices.Equal(got.Recovered, []int64{11}) || len(got.Broken) != 0 {
		t.Errorf("Recover = %+v", got)
	}
	if exists(dir, "11.part.wav") || !exists(dir, "11.wav") {
		t.Fatal("not renamed to .wav")
	}
	fin, err := s.Finished()
	if err != nil || len(fin) != 1 {
		t.Fatalf("Finished = %v, %v", fin, err)
	}
	if fin[0].EndReason != mic.EndInterrupted || !fin[0].EndedAt.Equal(t0.Add(3*time.Second)) {
		t.Errorf("recovered = %+v", fin[0])
	}
	f, _ := os.Open(filepath.Join(dir, "11.wav"))
	defer f.Close()
	if info, perr := mic.Parse(f); perr != nil || info.DataBytes != 3*32000 {
		t.Errorf("Parse = %+v, %v", info, perr)
	}

	// A second Recover finds nothing to do.
	again, err := s.Recover()
	if err != nil || len(again.Recovered) != 0 || len(again.Broken) != 0 {
		t.Errorf("second Recover = %+v, %v", again, err)
	}
}

func TestRecoverRenamedButSidecarNotUpdated(t *testing.T) {
	s, dir, _ := newSpool(t)
	crash(t, s, rec(12, t0), 32000)
	// Simulate a crash between Finish's rename and the sidecar update.
	if _, err := s.finalize(12); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recover()
	if err != nil || !slices.Equal(got.Recovered, []int64{12}) {
		t.Fatalf("Recover = %+v, %v", got, err)
	}
	m := readSidecar(t, dir, 12)
	if m["end_reason"] != "interrupted" || m["ended_at"] != "2026-11-20T09:30:01.123456789Z" {
		t.Errorf("sidecar = %v", m)
	}
}

func TestRecoverKeepsFinished(t *testing.T) {
	s, dir, _ := newSpool(t)
	record(t, s, rec(13, t0), make([]byte, 32))
	if err := s.Finish(13, t0.Add(time.Second), mic.EndTap); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recover()
	if err != nil || len(got.Recovered) != 0 || len(got.Broken) != 0 {
		t.Fatalf("Recover = %+v, %v", got, err)
	}
	if m := readSidecar(t, dir, 13); m["end_reason"] != "tap" {
		t.Errorf("finished sidecar changed: %v", m)
	}
}

func TestRecoverMovesOrphansToBroken(t *testing.T) {
	s, dir, _ := newSpool(t)
	sidecar := func(id int64) []byte {
		b, err := json.Marshal(rec(id, t0))
		if err != nil {
			t.Fatal(err)
		}

		return b
	}
	write(t, dir, "21.json", sidecar(21))                     // sidecar without audio
	write(t, dir, "22.part.wav", mic.Header(0))               // audio without sidecar
	write(t, dir, "23.wav", mic.Header(0))                    // finished audio without sidecar
	write(t, dir, "24.json", []byte("not json"))              // unreadable sidecar ...
	write(t, dir, "24.part.wav", mic.Header(0))               // ... and its audio
	write(t, dir, "25.json", sidecar(25))                     // sidecar ...
	write(t, dir, "25.part.wav", []byte("RIFF"))              // ... with audio shorter than a header
	write(t, dir, "26.json.tmp", []byte("{"))                 // interrupted sidecar update
	write(t, dir, "26.json", sidecar(26))                     // ... whose sidecar is intact
	write(t, dir, "26.part.wav", append(mic.Header(0), 0, 0)) // ... and audio recoverable
	write(t, dir, "x.wav", []byte("junk"))                    // not a recording ID
	write(t, dir, "notes.txt", []byte("left alone"))          // unrelated file
	write(t, dir, "27.json", sidecar(27))                     // both audio halves present
	write(t, dir, "27.part.wav", append(mic.Header(0), 0, 0)) // ...
	write(t, dir, "27.wav", append(mic.Header(2), 0, 0))      // ...
	if err := os.MkdirAll(filepath.Join(dir, "broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "broken"), "21.json", []byte("older")) // name collision

	got, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !slices.Equal(got.Recovered, []int64{26, 27}) {
		t.Errorf("Recovered = %v", got.Recovered)
	}
	wantBroken := []string{
		"21.json", "22.part.wav", "23.wav", "24.json", "24.part.wav",
		"25.json", "25.part.wav", "26.json.tmp", "27.part.wav", "x.wav",
	}
	if !slices.Equal(got.Broken, wantBroken) {
		t.Errorf("Broken = %v, want %v", got.Broken, wantBroken)
	}
	for _, name := range wantBroken {
		if exists(dir, name) {
			t.Errorf("%s still in the spool", name)
		}
	}
	bdir := filepath.Join(dir, "broken")
	for _, name := range []string{"21.json", "21.json.1", "22.part.wav", "26.json.tmp", "x.wav"} {
		if !exists(bdir, name) {
			t.Errorf("broken/%s missing", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(bdir, "21.json")); string(b) != "older" {
		t.Errorf("collision overwrote broken/21.json: %q", b)
	}
	if !exists(dir, "notes.txt") || !exists(dir, "26.wav") || !exists(dir, "27.wav") {
		t.Error("kept files missing")
	}
	if m := readSidecar(t, dir, 27); m["end_reason"] != "interrupted" {
		t.Errorf("27 sidecar = %v", m)
	}
}

func TestRecoverUnsearchableBrokenDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, dir, _ := newSpool(t)
	write(t, dir, "31.json", []byte("{}")) // sidecar without audio
	bdir := filepath.Join(dir, "broken")
	if err := os.Mkdir(bdir, 0o600); err != nil { // no search permission
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bdir, 0o700) })

	done := make(chan error, 1)
	go func() {
		_, err := s.Recover()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("unsearchable broken/: want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recover did not return")
	}
	if !exists(dir, "31.json") {
		t.Error("31.json lost")
	}
}
