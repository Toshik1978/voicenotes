package sqlite_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
)

// These tests drive the real pipeline.Worker and Ingester against a real
// temp database. They live here because package pipeline must not import the
// store; a test-only import in this direction is fine.

type countWaker struct{ n atomic.Int32 }

func (w *countWaker) Wake() { w.n.Add(1) }

func (w *countWaker) HoldAck(capture.Capture) func() { return func() {} }

func TestDuplicateIngestKeepsOneRowAndDoesNotWake(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	w := &countWaker{}
	ing, err := pipeline.NewIngester(s, w)
	if err != nil {
		t.Fatal(err)
	}

	first, err := ing.Ingest(ctx, newCapture(t, 7), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ing.Ingest(ctx, newCapture(t, 7), nil); !errors.Is(err, pipeline.ErrDuplicate) {
		t.Fatalf("redelivery: want ErrDuplicate, got %v", err)
	}
	if got := w.n.Load(); got != 1 {
		t.Errorf("worker woken %d times, want 1", got)
	}
	// Only one row exists: the next distinct message gets the very next ID.
	next, err := ing.Ingest(ctx, newCapture(t, 8), nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != first.ID+1 {
		t.Errorf("IDs %d then %d: duplicate left a row behind", first.ID, next.ID)
	}
}

// stepLog records which steps ran, across worker "processes".
type stepLog struct {
	mu    sync.Mutex
	steps []capture.Step
}

func (l *stepLog) add(s capture.Step) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, s)
}

func (l *stepLog) snapshot() []capture.Step {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]capture.Step(nil), l.steps...)
}

func handlers(l *stepLog, crashAt capture.Step, crash context.CancelFunc) map[capture.Step]pipeline.Handler {
	hs := map[capture.Step]pipeline.Handler{}
	for _, st := range allSteps() {
		hs[st] = func(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
			l.add(st)
			if st == crashAt {
				// The process dies while this step runs: nothing it
				// produced is persisted.
				crash()
			}

			return c, capture.OK()
		}
	}
	// collect never runs for the audio captures these tests queue; the
	// main lane just needs a handler for it.
	hs[capture.StepCollect] = func(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
		return c, capture.OK()
	}

	return hs
}

func runWorker(ctx context.Context, t *testing.T, s *sqlite.DB, hs map[capture.Step]pipeline.Handler) <-chan struct{} {
	t.Helper()

	w, err := pipeline.New(
		capture.LaneMain,
		s,
		pipeline.SystemClock{},
		capture.Policy{MaxInterval: time.Second},
		hs,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	w.Wake()

	return done
}

func allSteps() []capture.Step {
	return []capture.Step{
		capture.StepDownload, capture.StepExtract, capture.StepEnrich,
		capture.StepWriteNote, capture.StepFinalizeReply, capture.StepCleanup,
	}
}

// crashRun inserts a capture into a fresh DB in dir and runs a worker that
// "dies" while running crashAt, then closes the DB.
func crashRun(t *testing.T, dir string, l *stepLog, crashAt capture.Step) {
	t.Helper()

	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newCapture(t, 1)
	if err = s.Insert(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	ctx, crash := context.WithCancel(context.Background())
	<-runWorker(ctx, t, s, handlers(l, crashAt, crash))
	crash()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

// drain runs a worker on s until no unfinished capture is left.
func drain(t *testing.T, s *sqlite.DB, l *stepLog) {
	t.Helper()

	ctx, stop := context.WithCancel(context.Background())
	done := runWorker(ctx, t, s, handlers(l, "", stop))
	defer func() { stop(); <-done }()

	deadline := time.After(10 * time.Second)
	for {
		_, ok, err := s.OldestUnfinished(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return
		}
		select {
		case <-deadline:
			t.Fatal("capture never finished after restart")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestCrashBetweenStepsResumesAtStoredStep(t *testing.T) {
	steps := allSteps()
	for i, crashAt := range steps {
		t.Run(string(crashAt), func(t *testing.T) {
			dir := t.TempDir()
			l := &stepLog{}
			crashRun(t, dir, l, crashAt)

			// Second process: same directory, no crash.
			s := open(t, dir)
			stored, ok, err := s.OldestUnfinished(context.Background())
			if err != nil || !ok {
				t.Fatalf("capture lost across restart: ok=%v err=%v", ok, err)
			}
			if stored.Step != crashAt {
				t.Fatalf("stored step = %q, want %q", stored.Step, crashAt)
			}
			drain(t, s, l)

			// Steps up to and including the crashed one ran once; the crashed
			// step and everything after ran again; nothing earlier re-ran.
			want := slices.Concat(steps[:i+1], steps[i:])
			if got := l.snapshot(); !slices.Equal(got, want) {
				t.Fatalf("ran %v, want %v", got, want)
			}
		})
	}
}

// TestUpgradeFrom001KeepsQueuedCaptures builds a database at schema 001 with
// queued audio captures before and past transcription, opens it with the
// current store, and checks that migration 002 moved every file into
// capture_files, renamed the step, and that every capture still finishes.
func TestUpgradeFrom001KeepsQueuedCaptures(t *testing.T) {
	dir := t.TempDir()
	legacy, err := sqlite.OpenAtVersion(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).UTC().UnixNano()
	rows := []struct {
		id                        int64
		kind, step, status        string
		fileID, mime, audio, text any
		fileSize                  any
	}{
		{1, "audio", "download", "pending", "FAKE-1", "audio/ogg", nil, nil, 100},
		{2, "audio", "transcribe", "waiting", "FAKE-2", "audio/ogg", "tg--1001234567890-2-9.ogg", nil, 200},
		{3, "audio", "enrich", "pending", "FAKE-3", nil, "tg--1001234567890-3-9.ogg", "hello", nil},
		{4, "text", "write_note", "pending", nil, nil, nil, "note", nil},
		{5, "audio", "done", "done", nil, nil, nil, nil, nil},
	}
	for _, r := range rows {
		_, err = legacy.ExecContext(context.Background(), `INSERT INTO captures (
id, source, source_chat_id, source_message_id, kind, received_at, file_id, file_size, mime_type,
audio_path, text, step, status, next_attempt_at, updated_at)
VALUES (?, 'telegram', -1001234567890, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.id, r.kind, past, r.fileID, r.fileSize, r.mime, r.audio, r.text, r.step, r.status, past, past)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s := open(t, dir)
	if v := s.UserVersion(); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	file := func(id int64, fileID, mime, local string, size int64, state capture.FileState) []capture.File {
		return []capture.File{{
			CaptureID: id, Source: "telegram", SourceChatID: -1001234567890, SourceMessageID: id,
			Kind: capture.FileAudio, FileID: fileID, MimeType: mime, FileSize: size, LocalPath: local, State: state,
		}}
	}
	want := map[int64]struct {
		step  capture.Step
		files []capture.File
	}{
		1: {capture.StepDownload, file(1, "FAKE-1", "audio/ogg", "", 100, capture.FilePending)},
		2: {capture.StepExtract, file(2, "FAKE-2", "audio/ogg", "tg--1001234567890-2-9.ogg", 200, capture.FilePending)},
		3: {capture.StepEnrich, file(3, "FAKE-3", "", "tg--1001234567890-3-9.ogg", 0, capture.FileDone)},
		4: {capture.StepWriteNote, nil},
		5: {capture.StepDone, nil},
	}
	for id, w := range want {
		c, ok, gerr := s.Get(id)
		if gerr != nil || !ok {
			t.Fatalf("capture %d lost: ok=%v err=%v", id, ok, gerr)
		}
		for i := range c.Files {
			c.Files[i].ID = 0 // assigned by the migration
		}
		if c.Step != w.step || !reflect.DeepEqual(c.Files, w.files) {
			t.Errorf("capture %d: step %s files %+v\nwant step %s files %+v", id, c.Step, c.Files, w.step, w.files)
		}
		if c.AlbumID != "" {
			t.Errorf("capture %d: album %q, want empty", id, c.AlbumID)
		}
	}
	if c3, _, _ := s.Get(3); c3.Text != "hello" {
		t.Errorf("transcript = %q, want it kept in captures.text", c3.Text)
	}
	paths, err := s.FilePaths(context.Background())
	if err != nil || !slices.Equal(paths, []string{"tg--1001234567890-2-9.ogg", "tg--1001234567890-3-9.ogg"}) {
		t.Fatalf("FilePaths = %v err=%v: local audio must stay referenced", paths, err)
	}

	l := &stepLog{}
	drain(t, s, l)
	counts := map[capture.Step]int{}
	for _, st := range l.snapshot() {
		counts[st]++
	}
	wantCounts := map[capture.Step]int{
		capture.StepDownload: 1, capture.StepExtract: 2, capture.StepEnrich: 3,
		capture.StepWriteNote: 4, capture.StepFinalizeReply: 4, capture.StepCleanup: 4,
	}
	if !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("steps run %v, want %v", counts, wantCounts)
	}
}
