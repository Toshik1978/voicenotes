package pipeline_test

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// errStore is the injected store failure.
var errStore = errors.New("fake store: unavailable")

// fakeClock is a manual clock. After registers a waiter that fires only
// when the test advances time past its deadline; every call is announced on
// waits so tests can block until the worker is asleep.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
	waits   chan time.Duration
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, waits: make(chan time.Duration, 100)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.now
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	ch := make(chan time.Time, 1)
	f.waiters = append(f.waiters, waiter{at: f.now.Add(d), ch: ch})
	f.mu.Unlock()

	f.waits <- d

	return ch
}

// Advance moves time forward and fires every waiter that became due.
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.now = f.now.Add(d)
	kept := f.waiters[:0]
	for _, w := range f.waiters {
		if w.at.After(f.now) {
			kept = append(kept, w)
			continue
		}

		w.ch <- f.now
	}
	f.waiters = kept
}

// awaitSleep blocks until the worker calls After and returns the duration.
func (f *fakeClock) awaitSleep(t *testing.T) time.Duration {
	t.Helper()

	select {
	case d := <-f.waits:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("worker never slept on the clock")
		return 0
	}
}

// fakeStore is an in-memory queue. Like the sqlite store it serves the lane's
// due capture with the lowest ID first (FR-114a).
type fakeStore struct {
	mu        sync.Mutex
	rows      map[int64]capture.Capture
	updates   []capture.Capture
	nextCalls int
	nextErrs  int // fail this many NextDue calls
	updErrs   int // fail this many Update calls
	updated   chan capture.Capture
	idle      chan struct{} // signalled when NextDue finds nothing due
}

func newFakeStore(cs ...capture.Capture) *fakeStore {
	s := &fakeStore{
		rows:    map[int64]capture.Capture{},
		updated: make(chan capture.Capture, 100),
		idle:    make(chan struct{}, 100),
	}
	for i := range cs {
		s.rows[cs[i].ID] = cs[i]
	}

	return s
}

func (s *fakeStore) NextDue(_ context.Context, now time.Time, lane capture.Lane) (capture.Due, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextCalls++
	if s.nextErrs > 0 {
		s.nextErrs--
		return capture.Due{}, errStore
	}
	var due, waiting []capture.Capture
	for _, c := range s.rows { //nolint:gocritic // rangeValCopy: a test fake; the copy is harmless.
		if c.Status == capture.StatusDone || capture.LaneOf(c) != lane {
			continue
		}
		if c.NextAttemptAt.After(now) {
			waiting = append(waiting, c)
		} else {
			due = append(due, c)
		}
	}
	byID := func(a, b capture.Capture) int { return cmp.Compare(a.ID, b.ID) }
	if len(due) == 0 {
		s.idle <- struct{}{}
		if len(waiting) == 0 {
			return capture.Due{}, nil
		}
		// The lane's earliest wake time, like the real store.
		slices.SortFunc(waiting, func(a, b capture.Capture) int {
			return cmp.Or(a.NextAttemptAt.Compare(b.NextAttemptAt), byID(a, b))
		})

		return capture.Due{WakeAt: waiting[0].NextAttemptAt}, nil
	}
	slices.SortFunc(due, byID)
	head := due[0]

	return capture.Due{Capture: head, OK: true}, nil
}

func (s *fakeStore) Update(_ context.Context, c capture.Capture) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.updErrs > 0 {
		s.updErrs--
		return errStore
	}
	s.rows[c.ID] = c
	s.updates = append(s.updates, c)

	s.updated <- c

	return nil
}

// AdvanceCollect mirrors the store's compare-and-set: it writes next only
// while the row is still at collect with read's deadline and file count.
func (s *fakeStore) AdvanceCollect(ctx context.Context, read, next capture.Capture) (bool, error) {
	s.mu.Lock()
	cur := s.rows[read.ID]
	stale := cur.Step != capture.StepCollect || !cur.NextAttemptAt.Equal(read.NextAttemptAt) ||
		len(cur.Files) != len(read.Files)
	s.mu.Unlock()
	if stale {
		return false, nil
	}

	return true, s.Update(ctx, next)
}

// join adds f to the batch id and moves its deadline, as a late join does.
func (s *fakeStore) join(id int64, f capture.File, deadline time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := s.rows[id]
	c.Files = append(slices.Clone(c.Files), f)
	c.NextAttemptAt = deadline
	s.rows[id] = c
}

func (s *fakeStore) Insert(c capture.Capture) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rows[c.ID] = c
}

func (s *fakeStore) Updates() []capture.Capture {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]capture.Capture(nil), s.updates...)
}

func (s *fakeStore) NextCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.nextCalls
}

func (s *fakeStore) awaitUpdate(t *testing.T) capture.Capture {
	t.Helper()

	select {
	case c := <-s.updated:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no Update")
		return capture.Capture{}
	}
}

func (s *fakeStore) awaitIdle(t *testing.T) {
	t.Helper()

	select {
	case <-s.idle:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never found the queue idle")
	}
}
