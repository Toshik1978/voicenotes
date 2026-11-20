package recorder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/mic"
)

// waitTimeout bounds every wait on the code under test. It is a safety net
// against a hung test, never a pacing delay: the recorder only sees the
// manual clock.
const waitTimeout = 5 * time.Second

var errFake = errors.New("fake failure")

// ---------------------------------------------------------------- clock

// manualClock is a hand-written Clock. Time moves only when the test calls
// advance; every After or NewTicker registration is visible to waitFor so a
// test can block until the recorder is waiting on a given duration.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*clockWaiter
	changed chan struct{}
}

type clockWaiter struct {
	at     time.Time
	ch     chan time.Time
	d      time.Duration
	ticker bool
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now, changed: make(chan struct{})}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	return c.register(d, false).ch
}

func (c *manualClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	w := c.register(d, true)

	return w.ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.waiters = slices.DeleteFunc(c.waiters, func(x *clockWaiter) bool { return x == w })
	}
}

func (c *manualClock) register(d time.Duration, ticker bool) *clockWaiter {
	c.mu.Lock()
	defer c.mu.Unlock()

	w := &clockWaiter{at: c.now.Add(d), ch: make(chan time.Time, 1), d: d, ticker: ticker}
	c.waiters = append(c.waiters, w)
	close(c.changed)
	c.changed = make(chan struct{})

	return w
}

// advance moves time forward and fires every due waiter. One-shot waiters
// are removed; tickers fire once (dropping like time.Ticker) and re-arm.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
	kept := c.waiters[:0]

	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}

		select {
		case w.ch <- c.now:
		default:
		}

		if w.ticker {
			w.at = c.now.Add(w.d)
			kept = append(kept, w)
		}
	}

	c.waiters = kept
}

// waitFor blocks until something waits on the clock for exactly d.
func (c *manualClock) waitFor(t *testing.T, d time.Duration) {
	t.Helper()

	deadline := time.After(waitTimeout)

	for {
		c.mu.Lock()
		found := slices.ContainsFunc(c.waiters, func(w *clockWaiter) bool { return w.d == d })
		ch := c.changed
		c.mu.Unlock()

		if found {
			return
		}

		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("nothing waited on the clock for %v", d)
		}
	}
}

// ---------------------------------------------------------------- keys

type fakeKeys struct {
	mu      sync.Mutex
	opened  chan string
	cur     chan KeyEvent
	errs    int // fail this many Opens
	grabbed bool
	closes  int
}

func newFakeKeys() *fakeKeys {
	return &fakeKeys{opened: make(chan string, 16), grabbed: true}
}

func (k *fakeKeys) Open(path string) (KeyStream, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.opened <- path

	if k.errs > 0 {
		k.errs--
		return KeyStream{}, errFake
	}

	ch := make(chan KeyEvent)
	k.cur = ch

	return KeyStream{
		Events:  ch,
		Grabbed: k.grabbed,
		Close: func() error {
			k.mu.Lock()
			defer k.mu.Unlock()
			k.closes++

			return nil
		},
	}, nil
}

// send delivers ev and returns once the recorder has taken it.
func (k *fakeKeys) send(t *testing.T, ev KeyEvent) {
	t.Helper()

	k.mu.Lock()
	ch := k.cur
	k.mu.Unlock()

	select {
	case ch <- ev:
	case <-time.After(waitTimeout):
		t.Fatalf("recorder did not take key event %v", ev)
	}
}

func (k *fakeKeys) closeCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.closes
}

// ---------------------------------------------------------------- audio

type fakeAudio struct {
	mu     sync.Mutex
	opened chan mic.Receiver
	q      *FrameQueue
	lost   chan error
	errs   int
	closes int
}

func newFakeAudio() *fakeAudio { return &fakeAudio{opened: make(chan mic.Receiver, 16)} }

func (a *fakeAudio) Open(rcv mic.Receiver, q *FrameQueue) (AudioStream, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.opened <- rcv

	if a.errs > 0 {
		a.errs--
		return AudioStream{}, errFake
	}

	a.q = q
	a.lost = make(chan error, 1)

	return AudioStream{
		Lost: a.lost,
		Close: func() error {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.closes++

			return nil
		},
	}, nil
}

func (a *fakeAudio) queue() *FrameQueue {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.q
}

func (a *fakeAudio) lose() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.lost <- fmt.Errorf("stopped: %w", ErrDeviceLost)
}

// ---------------------------------------------------------------- discovery

type discoverResult struct {
	err error
	rcv mic.Receiver
	ok  bool
}

type fakeDiscovery struct {
	mu     sync.Mutex
	script []discoverResult // consumed in order; the last one repeats
	calls  chan struct{}
}

func newFakeDiscovery(script ...discoverResult) *fakeDiscovery {
	return &fakeDiscovery{script: script, calls: make(chan struct{}, 64)}
}

func (d *fakeDiscovery) Discover() (mic.Receiver, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.calls <- struct{}{}
	r := d.script[0]

	if len(d.script) > 1 {
		d.script = d.script[1:]
	}

	return r.rcv, r.ok, r.err
}

// ---------------------------------------------------------------- spool

type memSpool struct {
	mu        sync.Mutex
	events    chan string
	lists     atomic.Int64
	audio     map[int64]*memWriter
	done      map[int64]Recording
	free      int64
	freeErr   error
	recErr    error
	beginErrs int
	listErrs  int
	openErrs  int
	rmErrs    int
	writeErr  error
}

func newMemSpool() *memSpool {
	return &memSpool{
		events: make(chan string, 128),
		audio:  map[int64]*memWriter{},
		done:   map[int64]Recording{},
		free:   1 << 30,
	}
}

type memWriter struct {
	s      *memSpool
	buf    bytes.Buffer
	closed bool
}

func (w *memWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()

	if w.s.writeErr != nil {
		return 0, w.s.writeErr
	}

	return w.buf.Write(p)
}

func (w *memWriter) Close() error {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	w.closed = true

	return nil
}

func (s *memSpool) emitf(format string, args ...any) { s.events <- fmt.Sprintf(format, args...) }

func (s *memSpool) Recover() (Recovery, error) {
	s.emitf("recover")
	return Recovery{Recovered: []int64{9}}, s.recErr
}

func (s *memSpool) Begin(r Recording) (io.WriteCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.beginErrs > 0 {
		s.beginErrs--
		return nil, errFake
	}

	w := &memWriter{s: s}
	s.audio[r.ID] = w
	s.done[r.ID] = r
	s.emitf("begin %d %s %s", r.ID, r.Recorder, r.StartedAt.Format(time.RFC3339Nano))

	return w, nil
}

func (s *memSpool) Finish(id int64, endedAt time.Time, reason mic.EndReason) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	w := s.audio[id]
	r := s.done[id]
	r.EndedAt, r.EndReason = endedAt, reason
	s.done[id] = r
	s.emitf("finish %d %s closed=%t at=%s", id, reason, w != nil && w.closed, endedAt.Format(time.RFC3339Nano))

	return nil
}

func (s *memSpool) Discard(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.done, id)
	delete(s.audio, id)
	s.emitf("discard %d", id)

	return nil
}

// add puts a finished recording straight into the spool.
func (s *memSpool) add(id int64, startedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w := &memWriter{s: s, closed: true}
	w.buf.WriteString("audio-" + idName(id))
	s.audio[id] = w
	s.done[id] = Recording{ID: id, Recorder: "kitchen", StartedAt: startedAt, EndedAt: startedAt, EndReason: mic.EndTap}
}

func (s *memSpool) Finished() ([]Recording, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.lists.Add(1)

	if s.listErrs > 0 {
		s.listErrs--
		return nil, fmt.Errorf("decode sidecar 5: %w", errFake)
	}

	var out []Recording

	for _, r := range s.done {
		if r.EndReason != "" {
			out = append(out, r)
		}
	}

	slices.SortFunc(out, func(a, b Recording) int { return a.StartedAt.Compare(b.StartedAt) })

	return out, nil
}

func (s *memSpool) Open(id int64) (io.ReadSeekCloser, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.openErrs > 0 {
		s.openErrs--
		return nil, 0, errFake
	}

	b := s.audio[id].buf.Bytes()
	s.emitf("open %d", id)

	return nopSeekCloser{bytes.NewReader(b)}, int64(len(b)), nil
}

func (s *memSpool) Remove(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rmErrs > 0 {
		s.rmErrs--
		return errFake
	}

	delete(s.done, id)
	delete(s.audio, id)
	s.emitf("remove %d", id)

	return nil
}

func (s *memSpool) FreeBytes() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.free, s.freeErr
}

func (s *memSpool) setFree(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.free = n
}

type nopSeekCloser struct{ io.ReadSeeker }

func (nopSeekCloser) Close() error { return nil }

// ---------------------------------------------------------------- handoff

type fakeHandoff struct {
	mu      sync.Mutex
	results []Result // consumed in order; Stored once exhausted
	calls   chan int64
	bodies  [][]byte
}

func newFakeHandoff(results ...Result) *fakeHandoff {
	return &fakeHandoff{results: results, calls: make(chan int64, 64)}
}

func (h *fakeHandoff) Upload(_ context.Context, rec Recording, body io.ReadSeeker, size int64) (Result, error) {
	b, err := io.ReadAll(body)
	if err != nil || int64(len(b)) != size {
		return Retry, fmt.Errorf("short body: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.bodies = append(h.bodies, b)
	h.calls <- rec.ID

	res := Stored
	if len(h.results) > 0 {
		res, h.results = h.results[0], h.results[1:]
	}

	if res == Stored || res == Duplicate {
		return res, nil
	}

	return res, errFake
}

func (h *fakeHandoff) body(i int) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.bodies[i]
}

// ---------------------------------------------------------------- notifier, check-in

type startNotice struct {
	at time.Time
	id int64
}

type fakeNotifier struct {
	started   chan startNotice
	discarded chan int64
	block     chan struct{} // when set, Started waits for it (or ctx)
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{started: make(chan startNotice, 16), discarded: make(chan int64, 16)}
}

func (n *fakeNotifier) Started(ctx context.Context, id int64, startedAt time.Time) error {
	n.started <- startNotice{id: id, at: startedAt}

	if n.block != nil {
		select {
		case <-n.block:
		case <-ctx.Done():
		}
	}

	return errFake // failures must never matter
}

func (n *fakeNotifier) Discarded(_ context.Context, id int64) error {
	n.discarded <- id
	return errFake
}

type fakeCheckIn struct{ sent chan Status }

func (c *fakeCheckIn) Send(_ context.Context, s Status) error {
	c.sent <- s
	return errFake
}

// ---------------------------------------------------------------- harness

var start = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC) //nolint:gochecknoglobals // test fixture

var kitchen = mic.Receiver{Card: 2, InputPath: "/dev/input/event7"} //nolint:gochecknoglobals // test fixture

type harness struct {
	r      *Recorder
	clk    *manualClock
	keys   *fakeKeys
	audio  *fakeAudio
	disc   *fakeDiscovery
	spool  *memSpool
	hand   *fakeHandoff
	note   *fakeNotifier
	check  *fakeCheckIn
	logs   *observer.ObservedLogs
	core   zapcore.Core
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	core, logs := observer.New(zapcore.DebugLevel)

	h := &harness{
		clk:   newManualClock(start),
		keys:  newFakeKeys(),
		audio: newFakeAudio(),
		disc:  newFakeDiscovery(discoverResult{rcv: kitchen, ok: true}),
		spool: newMemSpool(),
		hand:  newFakeHandoff(),
		note:  newFakeNotifier(),
		check: &fakeCheckIn{sent: make(chan Status, 64)},
		logs:  logs,
		core:  core,
		done:  make(chan error, 1),
	}
	h.build(t)

	return h
}

// build (re)creates the recorder over the harness's current fakes.
func (h *harness) build(t *testing.T) {
	t.Helper()

	var ids atomic.Int64

	r, err := New(Deps{
		Keys: h.keys, Audio: h.audio, Discovery: h.disc, Spool: h.spool,
		Handoff: h.hand, Notifier: h.note, CheckIn: h.check, Log: zap.New(h.core),
	}, Options{
		Recorder:         "kitchen",
		Debounce:         500 * time.Millisecond,
		MinLength:        2 * time.Second,
		MaxLength:        10 * time.Second,
		SpoolMinFree:     1000,
		CheckinInterval:  time.Minute,
		RetryMaxInterval: 5 * time.Minute,
		FrameBuffer:      4,
		Clock:            h.clk,
		NewID:            func() int64 { return ids.Add(1) },
		Rand:             func() float64 { return 0.75 }, // jitter factor 1.1
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.r = r
}

// run starts the recorder; cleanup cancels it and waits for Run to return.
func (h *harness) run(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel

	go func() { h.done <- h.r.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(waitTimeout):
			t.Error("Run did not return after cancel")
		}
	})
	expectEvent(t, h.spool.events, "recover")
}

// session runs the recorder and waits until the receiver's keys are open
// and the 1 s tick is armed.
func (h *harness) session(t *testing.T) {
	t.Helper()
	h.run(t)
	expect(t, h.keys.opened, kitchen.InputPath)
	h.clk.waitFor(t, time.Second)
}

func (h *harness) beginEvent(id int64, at time.Time) string {
	return fmt.Sprintf("begin %d kitchen %s", id, at.Format(time.RFC3339Nano))
}

func (h *harness) finishEvent(id int64, reason mic.EndReason, at time.Time) string {
	return fmt.Sprintf("finish %d %s closed=true at=%s", id, reason, at.Format(time.RFC3339Nano))
}

func expect[T comparable](t *testing.T, ch <-chan T, want T) {
	t.Helper()

	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("got %v, want %v", got, want)
		}
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %v", want)
	}
}

func expectEvent(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	expect(t, ch, want)
}

func noEvent[T any](t *testing.T, ch <-chan T) {
	t.Helper()

	select {
	case got := <-ch:
		t.Fatalf("unexpected %v", got)
	default:
	}
}

// eventually spins (without sleeping) until cond holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}

		runtime.Gosched()
	}
}

// ---------------------------------------------------------------- tests

func TestNewValidates(t *testing.T) {
	h := newHarness(t)
	full := Deps{
		Keys: h.keys, Audio: h.audio, Discovery: h.disc, Spool: h.spool,
		Handoff: h.hand, Notifier: h.note, CheckIn: h.check,
	}
	opts := Options{
		Recorder: "kitchen", MaxLength: time.Minute, CheckinInterval: time.Minute,
		RetryMaxInterval: time.Minute,
	}

	if _, err := New(full, opts); err != nil {
		t.Fatalf("valid: %v", err)
	}

	cases := map[string]func(*Deps, *Options){
		"no keys":      func(d *Deps, _ *Options) { d.Keys = nil },
		"no audio":     func(d *Deps, _ *Options) { d.Audio = nil },
		"no discovery": func(d *Deps, _ *Options) { d.Discovery = nil },
		"no spool":     func(d *Deps, _ *Options) { d.Spool = nil },
		"no handoff":   func(d *Deps, _ *Options) { d.Handoff = nil },
		"no notifier":  func(d *Deps, _ *Options) { d.Notifier = nil },
		"no check-in":  func(d *Deps, _ *Options) { d.CheckIn = nil },
		"no name":      func(_ *Deps, o *Options) { o.Recorder = "" },
		"no max":       func(_ *Deps, o *Options) { o.MaxLength = 0 },
		"no checkin":   func(_ *Deps, o *Options) { o.CheckinInterval = 0 },
		"no retry max": func(_ *Deps, o *Options) { o.RetryMaxInterval = 0 },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			d, o := full, opts
			mod(&d, &o)

			if _, err := New(d, o); err == nil {
				t.Fatal("New accepted an incomplete setup")
			}
		})
	}
}

func TestFrameQueueDropsWhenFull(t *testing.T) {
	q := NewFrameQueue(2)
	chunk := make([]byte, 4*mic.FrameSize)

	for range 2 {
		if !q.Offer(chunk) {
			t.Fatal("offer below capacity failed")
		}
	}

	if q.Offer(chunk) || q.Offer(chunk[:mic.FrameSize]) {
		t.Fatal("offer to a full queue succeeded")
	}

	if got := q.Dropped(); got != 5 {
		t.Fatalf("Dropped = %d frames, want 5", got)
	}
}

func TestRecoverRunsFirstAndRediscoversEvery5s(t *testing.T) {
	h := newHarness(t)
	h.spool.recErr = errFake // a failed recovery is logged; the recorder runs on
	h.disc = newFakeDiscovery(
		discoverResult{},
		discoverResult{err: errFake},
		discoverResult{rcv: kitchen, ok: true},
	)
	h.build(t)
	h.run(t) // asserts "recover" is the first spool call

	expect(t, h.disc.calls, struct{}{})
	h.clk.waitFor(t, 5*time.Second)

	if s := h.r.Status(); !s.Running || s.ReceiverPresent {
		t.Fatalf("status with no receiver = %+v", s)
	}

	noEvent(t, h.keys.opened)
	h.clk.advance(5 * time.Second)
	expect(t, h.disc.calls, struct{}{})
	h.clk.waitFor(t, 5*time.Second)
	h.clk.advance(5 * time.Second)
	expect(t, h.disc.calls, struct{}{})
	expect(t, h.keys.opened, kitchen.InputPath)
	eventually(t, "receiver present and grabbed", func() bool {
		s := h.r.Status()
		return s.ReceiverPresent && s.InputGrabbed
	})
}

func TestReportsUngrabbedInput(t *testing.T) {
	h := newHarness(t)
	h.keys.grabbed = false
	h.session(t)
	eventually(t, "present, not grabbed", func() bool {
		s := h.r.Status()
		return s.ReceiverPresent && !s.InputGrabbed
	})
}

func TestKeyOpenFailureRediscovers(t *testing.T) {
	h := newHarness(t)
	h.keys.errs = 1
	h.run(t)
	expect(t, h.keys.opened, kitchen.InputPath)
	h.clk.waitFor(t, 5*time.Second)
	h.clk.advance(5 * time.Second)
	expect(t, h.keys.opened, kitchen.InputPath)
	h.clk.waitFor(t, time.Second)
}

func TestTapStartsAndStopsRecording(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	expect(t, h.audio.opened, kitchen)
	expect(t, h.note.started, startNotice{id: 1, at: start})
	eventually(t, "status recording", func() bool { return h.r.Status().Recording })

	q := h.audio.queue()
	q.Offer([]byte("ab"))
	q.Offer([]byte("cd"))

	h.clk.advance(3 * time.Second)
	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.finishEvent(1, mic.EndTap, start.Add(3*time.Second)))

	// The finished recording goes straight to the hand-off.
	expect(t, h.hand.calls, 1)

	if got := string(h.hand.body(0)); got != "abcd" {
		t.Fatalf("audio = %q, want abcd", got)
	}
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "remove 1")

	if s := h.r.Status(); s.Recording {
		t.Fatalf("still recording: %+v", s)
	}

	noEvent(t, h.note.discarded)
}

func TestSlowStartNoticeNeverBlocksRecording(t *testing.T) {
	h := newHarness(t)
	h.note.block = make(chan struct{}) // never released
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	expect(t, h.note.started, startNotice{id: 1, at: start})
	h.clk.advance(3 * time.Second)
	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.finishEvent(1, mic.EndTap, start.Add(3*time.Second)))
}

func TestShortRecordingIsDiscarded(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	h.clk.advance(time.Second)
	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, "discard 1")
	expect(t, h.note.discarded, 1)
}

func TestTapRefusedWhenStorageLow(t *testing.T) {
	h := newHarness(t)
	h.spool.setFree(999) // below SpoolMinFree
	h.session(t)

	h.keys.send(t, KeyTap)
	h.keys.send(t, KeyTap) // taken only after the first was handled

	if s := h.r.Status(); !s.StorageFull {
		t.Fatalf("status = %+v, want storage full", s)
	}

	h.spool.setFree(1000)
	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))

	if s := h.r.Status(); s.StorageFull {
		t.Fatalf("status = %+v, want storage not full", s)
	}
}

func TestTapRefusedWhenFreeSpaceUnknown(t *testing.T) {
	h := newHarness(t)
	h.spool.freeErr = errFake
	h.session(t)

	h.keys.send(t, KeyTap)
	h.keys.send(t, KeyTap)
	noEvent(t, h.spool.events)

	if s := h.r.Status(); !s.StorageFull {
		t.Fatalf("status = %+v, want storage full", s)
	}
}

func TestMaxLengthStopsRecording(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	h.clk.advance(10 * time.Second)
	expectEvent(t, h.spool.events, h.finishEvent(1, mic.EndMaxLength, start.Add(10*time.Second)))
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "remove 1")

	// The next tap starts a new recording.
	h.clk.advance(time.Second)
	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(2, start.Add(11*time.Second)))
}

func TestKeyLossStopsRecordingAndRediscovers(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	h.clk.advance(3 * time.Second)
	h.keys.send(t, KeyLost)
	expectEvent(t, h.spool.events, h.finishEvent(1, mic.EndReceiverLost, start.Add(3*time.Second)))
	eventually(t, "keys closed, receiver gone", func() bool {
		return h.keys.closeCount() == 1 && !h.r.Status().ReceiverPresent
	})

	h.clk.waitFor(t, 5*time.Second)
	h.clk.advance(5 * time.Second)
	expect(t, h.keys.opened, kitchen.InputPath)
}

func TestClosedKeyStreamCountsAsLoss(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.mu.Lock()
	close(h.keys.cur)
	h.keys.mu.Unlock()

	eventually(t, "keys closed", func() bool { return h.keys.closeCount() == 1 })
	h.clk.waitFor(t, 5*time.Second)
}

func TestAudioLossStopsRecording(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	expect(t, h.audio.opened, kitchen)
	h.clk.advance(3 * time.Second)
	h.audio.lose()
	expectEvent(t, h.spool.events, h.finishEvent(1, mic.EndReceiverLost, start.Add(3*time.Second)))
	h.clk.waitFor(t, 5*time.Second)
}

func TestLossOfShortRecordingDiscards(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	h.keys.send(t, KeyLost)
	expectEvent(t, h.spool.events, "discard 1")
	expect(t, h.note.discarded, 1)
}

func TestShutdownFinishesRecordingAsInterrupted(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	h.clk.advance(time.Second) // below the minimum: still kept
	h.cancel()
	expectEvent(t, h.spool.events, h.finishEvent(1, mic.EndInterrupted, start.Add(time.Second)))

	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}

		h.done <- err // for the cleanup
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return")
	}

	if s := h.r.Status(); s.Running || s.Recording || s.ReceiverPresent {
		t.Fatalf("status after shutdown = %+v", s)
	}
}

func TestBeginFailureResetsToggle(t *testing.T) {
	h := newHarness(t)
	h.spool.beginErrs = 1
	h.session(t)

	h.keys.send(t, KeyTap)
	eventually(t, "start failure logged", func() bool {
		return h.logs.FilterMessage("recording could not start").Len() == 1
	})
	h.clk.advance(time.Second)
	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(2, start.Add(time.Second)))
	expect(t, h.audio.opened, kitchen) // only once, for recording 2
	noEvent(t, h.audio.opened)
}

func TestAudioOpenFailureDiscardsSpool(t *testing.T) {
	h := newHarness(t)
	h.audio.errs = 1
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	expectEvent(t, h.spool.events, "discard 1")
	eventually(t, "start failure logged", func() bool {
		return h.logs.FilterMessage("recording could not start").Len() == 1
	})
	noEvent(t, h.note.started)

	h.clk.advance(time.Second)
	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(2, start.Add(time.Second)))
}

func TestWriteFailureStopsRecording(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	expect(t, h.audio.opened, kitchen)
	h.spool.mu.Lock()
	h.spool.writeErr = errFake
	h.spool.mu.Unlock()
	h.clk.advance(3 * time.Second)
	h.audio.queue().Offer([]byte("ab"))
	expectEvent(t, h.spool.events, h.finishEvent(1, mic.EndInterrupted, start.Add(3*time.Second)))
}

func TestDroppedFramesAreLogged(t *testing.T) {
	h := newHarness(t)
	h.session(t)

	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.beginEvent(1, start))
	expect(t, h.audio.opened, kitchen)

	q := h.audio.queue()
	q.dropped.Add(7)
	h.clk.advance(3 * time.Second)
	h.keys.send(t, KeyTap)
	expectEvent(t, h.spool.events, h.finishEvent(1, mic.EndTap, start.Add(3*time.Second)))

	entries := h.logs.FilterMessage("audio frames dropped").All()
	if len(entries) != 1 || entries[0].ContextMap()["frames"] != uint64(7) {
		t.Fatalf("drop log = %+v", entries)
	}
}

// ---------------------------------------------------------------- hand-off

// handoffHarness runs a recorder with no receiver, so only the hand-off and
// check-in loops do anything.
func handoffHarness(t *testing.T, results ...Result) *harness {
	t.Helper()

	h := newHarness(t)
	h.disc = newFakeDiscovery(discoverResult{})
	h.hand = newFakeHandoff(results...)

	h.build(t)

	return h
}

func TestHandoffOldestFirstDeletesOnStoredOrDuplicate(t *testing.T) {
	h := handoffHarness(t, Duplicate, Stored)
	h.spool.add(2, start.Add(time.Minute))
	h.spool.add(1, start)
	h.run(t)

	expect(t, h.hand.calls, 1)

	if got := string(h.hand.body(0)); got != "audio-1" {
		t.Fatalf("body = %q", got)
	}

	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "remove 1")
	expect(t, h.hand.calls, 2)
	expectEvent(t, h.spool.events, "open 2")
	expectEvent(t, h.spool.events, "remove 2")
	eventually(t, "nothing pending", func() bool {
		s := h.r.Status()
		return s.Pending == 0 && s.OldestPendingAt.IsZero()
	})
}

func TestHandoffRetryBacksOff(t *testing.T) {
	h := handoffHarness(t, Retry, Retry, Stored)
	h.spool.add(1, start)
	h.run(t)

	expect(t, h.hand.calls, 1)
	h.clk.waitFor(t, capture.Backoff(1, 5*time.Minute, func() float64 { return 0.75 })) // 5.5 s

	if s := h.r.Status(); s.Pending != 1 || !s.OldestPendingAt.Equal(start) {
		t.Fatalf("status = %+v", s)
	}

	h.clk.advance(5500 * time.Millisecond)
	expect(t, h.hand.calls, 1)
	h.clk.waitFor(t, 11*time.Second)
	h.clk.advance(11 * time.Second)
	expect(t, h.hand.calls, 1)
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "remove 1")
}

func TestHandoffMisconfiguredRetriesAtMaxInterval(t *testing.T) {
	h := handoffHarness(t, Misconfigured, Stored)
	h.spool.add(1, start)
	h.run(t)

	expect(t, h.hand.calls, 1)
	h.clk.waitFor(t, 5*time.Minute)

	if s := h.r.Status(); !s.Misconfigured || s.Pending != 1 {
		t.Fatalf("status = %+v", s)
	}

	h.clk.advance(5 * time.Minute)
	expect(t, h.hand.calls, 1)
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "remove 1")

	if s := h.r.Status(); s.Misconfigured {
		t.Fatalf("still misconfigured after a store: %+v", s)
	}
}

func TestHandoffKeepsAndSkipsRejected(t *testing.T) {
	h := handoffHarness(t, Rejected, Stored)
	h.spool.add(1, start)
	h.spool.add(2, start.Add(time.Minute))
	h.run(t)

	expect(t, h.hand.calls, 1)
	expect(t, h.hand.calls, 2)
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "open 2")
	expectEvent(t, h.spool.events, "remove 2")
	eventually(t, "one rejected, none pending", func() bool {
		s := h.r.Status()
		return s.Rejected == 1 && s.Pending == 0
	})

	// A new recording is handed over; the rejected one is not retried.
	h.spool.add(3, start.Add(2*time.Minute))
	h.r.wakeHandoff()
	expect(t, h.hand.calls, 3)
	expectEvent(t, h.spool.events, "open 3")
	expectEvent(t, h.spool.events, "remove 3")
	noEvent(t, h.hand.calls)
}

func TestHandoffListingFailureIsLoggedAndRetried(t *testing.T) {
	h := handoffHarness(t)
	h.spool.listErrs = 1
	h.spool.add(1, start)
	h.run(t)

	h.clk.waitFor(t, 5500*time.Millisecond)

	if s := h.r.Status(); !s.SpoolError {
		t.Fatalf("status = %+v, want spool error", s)
	}

	if n := h.logs.FilterMessage("spool listing failed, hand-off paused").Len(); n != 1 {
		t.Fatalf("listing failure logged %d times", n)
	}

	h.clk.advance(5500 * time.Millisecond)
	expect(t, h.hand.calls, 1)
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "remove 1")
	eventually(t, "spool error cleared", func() bool { return !h.r.Status().SpoolError })
}

func TestHandoffOpenAndRemoveFailuresRetry(t *testing.T) {
	h := handoffHarness(t, Stored, Duplicate)
	h.spool.openErrs = 1
	h.spool.rmErrs = 1
	h.spool.add(1, start)
	h.run(t)

	h.clk.waitFor(t, 5500*time.Millisecond) // open failed
	h.clk.advance(5500 * time.Millisecond)
	expect(t, h.hand.calls, 1) // stored, but the delete fails
	h.clk.waitFor(t, 5500*time.Millisecond)
	h.clk.advance(5500 * time.Millisecond)
	expect(t, h.hand.calls, 1) // the re-upload is a duplicate
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "open 1")
	expectEvent(t, h.spool.events, "remove 1")
}

// ---------------------------------------------------------------- check-in

func TestCheckInSendsStatusEveryInterval(t *testing.T) {
	h := handoffHarness(t)
	h.spool.setFree(10)
	h.run(t)

	first := <-h.check.sent
	if !first.Running || first.ReceiverPresent || !first.StorageFull {
		t.Fatalf("first check-in = %+v", first)
	}

	h.clk.waitFor(t, time.Minute)
	h.spool.setFree(1 << 30)
	h.clk.advance(time.Minute)

	select {
	case s := <-h.check.sent:
		if s.StorageFull {
			t.Fatalf("second check-in = %+v", s)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no second check-in")
	}
}

// ---------------------------------------------------------------- real spool

func TestRecorderWithDirSpool(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()

	ds, err := NewDirSpool(dir, h.clk.Now)
	if err != nil {
		t.Fatalf("NewDirSpool: %v", err)
	}

	var ids atomic.Int64

	r, err := New(Deps{
		Keys: h.keys, Audio: h.audio, Discovery: h.disc, Spool: ds,
		Handoff: h.hand, Notifier: h.note, CheckIn: h.check, Log: zap.NewNop(),
	}, Options{
		Recorder: "kitchen", MinLength: time.Second, MaxLength: time.Minute,
		SpoolMinFree: 1, CheckinInterval: time.Minute, RetryMaxInterval: time.Minute,
		Clock: h.clk, NewID: func() int64 { return ids.Add(1) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	expect(t, h.keys.opened, kitchen.InputPath)
	h.keys.send(t, KeyTap)
	expect(t, h.audio.opened, kitchen)

	pcm := bytes.Repeat([]byte{1, 0}, 800)
	h.audio.queue().Offer(pcm)
	h.clk.advance(2 * time.Second)
	h.keys.send(t, KeyTap)

	expect(t, h.hand.calls, 1)

	body := h.hand.body(0)
	if !bytes.Equal(body[:mic.HeaderSize], mic.Header(int64(len(pcm)))) || !bytes.Equal(body[mic.HeaderSize:], pcm) {
		t.Fatalf("uploaded WAV is wrong (%d bytes)", len(body))
	}

	eventually(t, "spool emptied", func() bool {
		recs, err := ds.Finished()
		return err == nil && len(recs) == 0 && !exists(dir, "1.json") && !exists(dir, "1.wav")
	})
}

func TestSystemClock(t *testing.T) {
	var c systemClock

	if c.Now().IsZero() {
		t.Fatal("zero time")
	}

	<-c.After(0)

	ch, stop := c.NewTicker(time.Hour)
	stop()

	if ch == nil {
		t.Fatal("nil ticker channel")
	}
}
