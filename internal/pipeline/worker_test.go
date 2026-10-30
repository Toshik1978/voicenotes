package pipeline_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

func t0() time.Time { return time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC) }

var testPolicy = capture.Policy{MaxInterval: 5 * time.Minute} //nolint:gochecknoglobals // test fixture

func textCapture(t *testing.T, id int64) capture.Capture {
	t.Helper()

	c, err := capture.New(capture.KindText, capture.StepEnrich, t0())
	if err != nil {
		t.Fatal(err)
	}
	c.ID = id

	return c
}

// call records one handler invocation.
type call struct {
	id   int64
	step capture.Step
}

// recorder builds handlers for every step that record calls and return the
// outcome chosen by result (OK when result is nil).
type recorder struct {
	mu     sync.Mutex
	calls  []call
	result func(c capture.Capture, n int) capture.Outcome
	check  func(c capture.Capture)
}

func (r *recorder) handlers() map[capture.Step]pipeline.Handler {
	h := func(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
		if r.check != nil {
			r.check(c)
		}
		r.mu.Lock()
		r.calls = append(r.calls, call{c.ID, c.Step})
		n := len(r.calls)
		r.mu.Unlock()
		if r.result != nil {
			return c, r.result(c, n)
		}

		return c, capture.OK()
	}

	return allSteps(h)
}

func (r *recorder) Calls() []call {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]call(nil), r.calls...)
}

func allSteps(h pipeline.Handler) map[capture.Step]pipeline.Handler {
	return map[capture.Step]pipeline.Handler{
		capture.StepDownload:      h,
		capture.StepTranscribe:    h,
		capture.StepEnrich:        h,
		capture.StepWriteNote:     h,
		capture.StepFinalizeReply: h,
		capture.StepCleanup:       h,
	}
}

func newWorker(t *testing.T, s pipeline.Store, c pipeline.Clock, h map[capture.Step]pipeline.Handler) *pipeline.Worker {
	t.Helper()

	w, err := pipeline.New(s, c, testPolicy, h, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return w
}

// start runs w in the background; the returned stop cancels it and waits.
func start(t *testing.T, w *pipeline.Worker) (stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop on cancel")
		}
	}
	t.Cleanup(stop)

	return stop
}

func TestNewValidates(t *testing.T) {
	ok := allSteps(func(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
		return c, capture.OK()
	})
	clock := newFakeClock(t0())

	for _, step := range []capture.Step{
		capture.StepDownload, capture.StepTranscribe, capture.StepEnrich,
		capture.StepWriteNote, capture.StepFinalizeReply, capture.StepCleanup,
	} {
		h := allSteps(ok[step])
		delete(h, step)
		_, err := pipeline.New(newFakeStore(), clock, testPolicy, h, nil)
		if err == nil || !strings.Contains(err.Error(), string(step)) {
			t.Errorf("missing %s: err = %v, want it named", step, err)
		}
		h[step] = nil
		if _, err := pipeline.New(newFakeStore(), clock, testPolicy, h, nil); err == nil {
			t.Errorf("nil handler for %s accepted", step)
		}
	}

	if _, err := pipeline.New(nil, clock, testPolicy, ok, nil); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := pipeline.New(newFakeStore(), nil, testPolicy, ok, nil); err == nil {
		t.Error("nil clock accepted")
	}
	if _, err := pipeline.New(newFakeStore(), clock, capture.Policy{}, ok, nil); err == nil {
		t.Error("zero MaxInterval accepted")
	}
	if _, err := pipeline.New(newFakeStore(), clock, testPolicy, ok, nil); err != nil {
		t.Errorf("valid worker (nil logger): %v", err)
	}
}

func TestRunProcessesFIFOAndPersistsEveryStep(t *testing.T) {
	store := newFakeStore(textCapture(t, 2), textCapture(t, 1))
	clock := newFakeClock(t0())
	rec := &recorder{}
	// FR-008: the step a handler sees must already be the persisted one.
	rec.check = func(c capture.Capture) {
		store.mu.Lock()
		persisted := store.rows[c.ID].Step
		store.mu.Unlock()
		if persisted != c.Step {
			t.Errorf("capture %d: running %s but store holds %s", c.ID, c.Step, persisted)
		}
	}
	w := newWorker(t, store, clock, rec.handlers())

	start(t, w)
	store.awaitIdle(t)

	steps := []capture.Step{
		capture.StepEnrich, capture.StepWriteNote, capture.StepFinalizeReply, capture.StepCleanup,
	}
	want := make([]call, 0, 2*len(steps))
	for _, id := range []int64{1, 2} {
		for _, s := range steps {
			want = append(want, call{id, s})
		}
	}
	got := rec.Calls()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}

	ups := store.Updates()
	if len(ups) != len(want) {
		t.Fatalf("updates = %d, want one per step (%d)", len(ups), len(want))
	}
	if last := ups[len(ups)-1]; last.ID != 2 || last.Status != capture.StatusDone {
		t.Errorf("last update = id %d %s, want id 2 done", last.ID, last.Status)
	}
	if !w.Alive() {
		t.Error("Alive() = false while running")
	}
}

func TestEmptyQueueWaitsForWakeOnly(t *testing.T) {
	store := newFakeStore()
	clock := newFakeClock(t0())
	rec := &recorder{}
	w := newWorker(t, store, clock, rec.handlers())

	start(t, w)
	store.awaitIdle(t)

	store.Insert(textCapture(t, 7))
	w.Wake()
	if c := store.awaitUpdate(t); c.ID != 7 || c.Step != capture.StepWriteNote {
		t.Errorf("update = id %d step %s", c.ID, c.Step)
	}
	store.awaitIdle(t) // drained again

	select {
	case d := <-clock.waits:
		t.Errorf("worker slept on the clock (%v) with an empty queue", d)
	default:
	}
}

func TestTemporaryOutcomeSleepsUntilWakeAt(t *testing.T) {
	store := newFakeStore(textCapture(t, 1))
	clock := newFakeClock(t0())
	rec := &recorder{result: func(_ capture.Capture, n int) capture.Outcome {
		if n == 1 {
			return capture.Temporary("http 503").On(capture.WaitingMemos)
		}
		return capture.OK()
	}}
	w := newWorker(t, store, clock, rec.handlers())
	start(t, w)

	// enrich temporary: waiting, backoff 5 s (no jitter), not persisted as OK.
	first := store.awaitUpdate(t)
	if first.Status != capture.StatusWaiting || first.Step != capture.StepEnrich ||
		first.Attempts != 1 || !first.NextAttemptAt.Equal(t0().Add(5*time.Second)) {
		t.Fatalf("after temporary: %+v", first)
	}
	if d := clock.awaitSleep(t); d != 5*time.Second {
		t.Fatalf("slept %v, want 5s until WakeAt", d)
	}
	if n := len(rec.Calls()); n != 1 {
		t.Fatalf("handler ran %d times before WakeAt", n)
	}

	clock.Advance(5 * time.Second)
	if c := store.awaitUpdate(t); c.Step != capture.StepWriteNote || c.Status != capture.StatusPending {
		t.Fatalf("after retry: step %s status %s", c.Step, c.Status)
	}
}

func TestWakeInterruptsTimedSleep(t *testing.T) {
	waiting := textCapture(t, 1)
	waiting.Status = capture.StatusWaiting
	waiting.NextAttemptAt = t0().Add(time.Hour)
	store := newFakeStore(waiting)
	clock := newFakeClock(t0())
	w := newWorker(t, store, clock, (&recorder{}).handlers())
	start(t, w)

	if d := clock.awaitSleep(t); d != time.Hour {
		t.Fatalf("slept %v, want 1h", d)
	}
	before := store.NextCalls()
	w.Wake()
	clock.awaitSleep(t) // re-checked the queue, still not due, slept again
	if store.NextCalls() <= before {
		t.Error("Wake did not trigger a queue check")
	}
}

func TestWakeIsNonBlockingAndCoalesces(t *testing.T) {
	w := newWorker(t, newFakeStore(), newFakeClock(t0()), (&recorder{}).handlers())
	done := make(chan struct{})
	go func() {
		for range 10 {
			w.Wake()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wake blocked without a running worker")
	}
}

func TestStoreErrorBacksOffAndKeepsRunning(t *testing.T) {
	store := newFakeStore(textCapture(t, 1))
	store.nextErrs = 2
	clock := newFakeClock(t0())
	w := newWorker(t, store, clock, (&recorder{}).handlers())
	start(t, w)

	if d := clock.awaitSleep(t); d != 5*time.Second {
		t.Fatalf("first store backoff %v, want 5s", d)
	}
	if !w.Alive() {
		t.Error("Alive() = false during store outage")
	}
	clock.Advance(5 * time.Second)
	if d := clock.awaitSleep(t); d != 10*time.Second {
		t.Fatalf("second store backoff %v, want 10s", d)
	}
	clock.Advance(10 * time.Second)
	if c := store.awaitUpdate(t); c.Step != capture.StepWriteNote {
		t.Fatalf("after recovery step = %s", c.Step)
	}
}

func TestUpdateErrorRetriesPersistWithoutRerunningStep(t *testing.T) {
	store := newFakeStore(textCapture(t, 1))
	store.updErrs = 1
	clock := newFakeClock(t0())
	rec := &recorder{}
	w := newWorker(t, store, clock, rec.handlers())
	start(t, w)

	if d := clock.awaitSleep(t); d != 5*time.Second {
		t.Fatalf("update backoff %v, want 5s", d)
	}
	if n := len(rec.Calls()); n != 1 {
		t.Fatalf("handler calls before retry = %d", n)
	}
	clock.Advance(5 * time.Second)
	if c := store.awaitUpdate(t); c.Step != capture.StepWriteNote {
		t.Fatalf("persisted step = %s, want write_note", c.Step)
	}
	store.awaitIdle(t)
	enrich := 0
	for _, c := range rec.Calls() {
		if c.step == capture.StepEnrich {
			enrich++
		}
	}
	if enrich != 1 {
		t.Errorf("enrich ran %d times, want 1 (result is re-persisted, not re-run)", enrich)
	}
}

func TestCancelMidStepDoesNotPersist(t *testing.T) {
	store := newFakeStore(textCapture(t, 1))
	ctx, cancel := context.WithCancel(context.Background())
	h := allSteps(func(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
		cancel()
		c.EnrichedJSON = "{}"
		return c, capture.OK()
	})
	w := newWorker(t, store, newFakeClock(t0()), h)

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if ups := store.Updates(); len(ups) != 0 {
		t.Errorf("persisted %d half-applied results", len(ups))
	}
	if w.Alive() {
		t.Error("Alive() = true after Run returned")
	}
}

func TestRunStopsPromptlyWhileSleeping(t *testing.T) {
	waiting := textCapture(t, 1)
	waiting.NextAttemptAt = t0().Add(time.Hour)
	store := newFakeStore(waiting)
	clock := newFakeClock(t0())
	w := newWorker(t, store, clock, (&recorder{}).handlers())
	if w.Alive() {
		t.Error("Alive() = true before Run")
	}
	stop := start(t, w)
	clock.awaitSleep(t)
	stop()
	if w.Alive() {
		t.Error("Alive() = true after stop")
	}
}

func TestUnknownStepIsRetriedNotDropped(t *testing.T) {
	odd := textCapture(t, 1)
	odd.Step = capture.Step("bogus")
	store := newFakeStore(odd)
	clock := newFakeClock(t0())
	w := newWorker(t, store, clock, (&recorder{}).handlers())
	start(t, w)

	c := store.awaitUpdate(t)
	if c.Status != capture.StatusWaiting || c.Step != "bogus" || c.Attempts != 1 ||
		!strings.Contains(c.LastError, "no handler") {
		t.Fatalf("unknown step result: %+v", c)
	}
}

func TestSystemClock(t *testing.T) {
	var c pipeline.Clock = pipeline.SystemClock{}
	before := time.Now()
	if got := c.Now(); got.Before(before) {
		t.Errorf("Now() = %v, before %v", got, before)
	}
	select {
	case <-c.After(time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Fatal("After never fired")
	}
}

func TestHandlerCannotChangeIdentityOrStep(t *testing.T) {
	cases := map[string]pipeline.Handler{
		"zero capture": func(_ context.Context, _ capture.Capture) (capture.Capture, capture.Outcome) {
			return capture.Capture{}, capture.Temporary("boom")
		},
		"advanced step": func(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
			c.ID, c.Step = 99, capture.StepCleanup
			return c, capture.OK()
		},
	}
	want := map[string]capture.Step{"zero capture": capture.StepEnrich, "advanced step": capture.StepWriteNote}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore(textCapture(t, 1))
			w := newWorker(t, store, newFakeClock(t0()), allSteps(h))
			start(t, w)

			c := store.awaitUpdate(t)
			if c.ID != 1 || c.Step != want[name] {
				t.Fatalf("update = id %d step %s, want id 1 step %s", c.ID, c.Step, want[name])
			}
		})
	}
}

func TestWorkerQueuesReplyEditOnceAndClearsFlag(t *testing.T) {
	c := textCapture(t, 1)
	c.Step, c.ReplyMessageID = capture.StepWriteNote, 42
	store := newFakeStore(c)
	clock := newFakeClock(t0())
	h := allSteps(func(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
		return c, capture.Temporary("down").On(capture.WaitingMemos)
	})
	w := newWorker(t, store, clock, h)
	var notified []capture.Capture
	var mu sync.Mutex
	w.SetQueueNotifier(func(_ context.Context, c capture.Capture) capture.Capture {
		mu.Lock()
		notified = append(notified, c)
		mu.Unlock()
		c.QueueReplyEdit = false

		return c
	})
	start(t, w)

	first := store.awaitUpdate(t)
	if !first.QueueReplyEdit || first.Status != capture.StatusWaiting {
		t.Fatalf("first update = %+v, want waiting with the flag persisted first", first)
	}
	if second := store.awaitUpdate(t); second.QueueReplyEdit {
		t.Fatal("flag not cleared durably after the edit")
	}
	// Advance only once the retry timer exists, or it never fires.
	clock.awaitSleep(t)

	// A later retry stays waiting and must not edit again.
	clock.Advance(10 * time.Minute)
	if u := store.awaitUpdate(t); u.QueueReplyEdit || u.Attempts != 2 {
		t.Fatalf("retry update = %+v, want attempt 2 without a new edit", u)
	}
	store.awaitIdle(t)
	mu.Lock()
	defer mu.Unlock()
	if len(notified) != 1 {
		t.Errorf("notifier calls = %d, want 1", len(notified))
	}
}

func TestWorkerCatchesUpQueuedEditAfterRestart(t *testing.T) {
	c := textCapture(t, 1)
	c.Status, c.QueueReplyEdit, c.WaitingOn = capture.StatusWaiting, true, capture.WaitingSTT
	store := newFakeStore(c)
	rec := &recorder{}
	w := newWorker(t, store, newFakeClock(t0()), rec.handlers())
	calls := 0
	w.SetQueueNotifier(func(_ context.Context, c capture.Capture) capture.Capture {
		calls++
		c.QueueReplyEdit = false

		return c
	})
	start(t, w)

	if u := store.awaitUpdate(t); u.QueueReplyEdit {
		t.Fatal("flag not cleared")
	}
	if calls != 1 {
		t.Errorf("notifier calls = %d, want 1", calls)
	}
}
