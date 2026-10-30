package sqlite_test

import (
	"context"
	"errors"
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

	return hs
}

func runWorker(ctx context.Context, t *testing.T, s *sqlite.DB, hs map[capture.Step]pipeline.Handler) <-chan struct{} {
	t.Helper()

	w, err := pipeline.New(s, pipeline.SystemClock{}, capture.Policy{MaxInterval: time.Second}, hs, nil)
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
		capture.StepDownload, capture.StepTranscribe, capture.StepEnrich,
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
