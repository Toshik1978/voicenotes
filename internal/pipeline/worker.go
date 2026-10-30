// Package pipeline runs captures through their steps. The Worker is the single
// FIFO loop over the durable queue (FR-011): it runs one step of the head
// capture, applies capture.Policy.Next and persists the result before
// anything else happens (FR-008). All I/O sits behind Store, Clock and the
// step handlers, so the loop is tested with fakes.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// Store is the slice of the capture queue the worker needs. NextDue must
// follow the strict-FIFO contract of capture.Due: only the lowest-ID
// unfinished capture is ever returned, and a head that is not due yet blocks
// later ones.
type Store interface {
	NextDue(ctx context.Context, now time.Time) (capture.Due, error)
	Update(ctx context.Context, c capture.Capture) error
}

// Clock is the worker's time source. After returns a channel that receives
// once d has elapsed, like time.After.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Handler performs one attempt at the capture's current step. It returns the
// capture with whatever fields the step produced and the attempt's outcome.
// ID and Step belong to the queue and the state machine: the worker restores
// them from the input, so a handler that returns a zero Capture (or moves the
// step itself) cannot misdirect the update or skip a step.
type Handler func(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome)

// SystemClock is the real Clock.
type SystemClock struct{}

// Now returns time.Now.
func (SystemClock) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// forever marks a sleep that only Wake or cancellation ends.
const forever time.Duration = -1

// steps lists every step that needs a handler; done is terminal.
func steps() []capture.Step {
	return []capture.Step{
		capture.StepDownload, capture.StepTranscribe, capture.StepEnrich,
		capture.StepWriteNote, capture.StepFinalizeReply, capture.StepCleanup,
	}
}

// Worker is the single pipeline loop. Run must be called at most once at a
// time; Wake, HoldAck and Alive are safe from any goroutine.
type Worker struct {
	store    Store
	clock    Clock
	policy   capture.Policy
	handlers map[capture.Step]Handler
	log      *zap.Logger
	notify   QueueNotifier

	wake  chan struct{}
	alive atomic.Bool
	// holds are the acknowledgements in flight (see HoldAck).
	holdsMu sync.Mutex
	holds   map[*ackHold]struct{}

	// pending is a step result that could not be persisted yet. It is
	// retried before any new step runs, so a completed step is never re-run
	// because of a store failure (Constitution VI). Only Run touches it.
	pending *capture.Capture
	// failures counts consecutive store errors for the retry backoff.
	failures int
}

// New validates the wiring and returns a Worker. Every step from download to
// cleanup needs a non-nil handler (enrich gets a pass-through when the LLM is
// not configured), so a missing handler fails at startup rather than at the
// first capture. policy.MaxInterval must be positive: it also caps the
// backoff after store errors. A nil log means no logging.
func New(
	store Store, clock Clock, policy capture.Policy, handlers map[capture.Step]Handler, log *zap.Logger,
) (*Worker, error) {
	if store == nil {
		return nil, errors.New("pipeline: nil store")
	}
	if clock == nil {
		return nil, errors.New("pipeline: nil clock")
	}
	if policy.MaxInterval <= 0 {
		return nil, errors.New("pipeline: policy MaxInterval must be positive")
	}
	hs := make(map[capture.Step]Handler, len(handlers))
	for _, s := range steps() {
		h := handlers[s]
		if h == nil {
			return nil, fmt.Errorf("pipeline: no handler for step %q", s)
		}
		hs[s] = h
	}
	if log == nil {
		log = zap.NewNop()
	}

	return &Worker{
		store:    store,
		clock:    clock,
		policy:   policy,
		handlers: hs,
		log:      log,
		wake:     make(chan struct{}, 1),
		holds:    map[*ackHold]struct{}{},
	}, nil
}

// SetQueueNotifier installs the hook that edits the reply of a capture that
// just went into waiting. Call it before Run; without one no edit happens.
func (w *Worker) SetQueueNotifier(n QueueNotifier) { w.notify = n }

// Wake asks the worker to re-check the queue now, typically after ingest
// inserted a capture. It never blocks, and wakes that arrive while one is
// already pending coalesce into one.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// AckHoldLimit bounds how long HoldAck keeps the worker off a capture. It
// exceeds one Bot API call (a minute) plus a single-row write, so a live
// acknowledgement always finishes first; it only matters if one hangs.
const AckHoldLimit = 2 * time.Minute

// sourceKey is a capture's source identity, known before its row exists.
type sourceKey struct {
	source        string
	chat, message int64
}

func keyOf(c capture.Capture) sourceKey {
	return sourceKey{source: c.Source, chat: c.SourceChatID, message: c.SourceMessageID}
}

// ackHold is one in-flight acknowledgement.
type ackHold struct {
	key   sourceKey
	until time.Time
}

// HoldAck keeps the worker off the capture with c's source identity while
// its acknowledgement is in flight, so finalize_reply edits that reply
// instead of sending a second one (FR-025). Call it before the insert, so no
// scan sees the row unheld. The hold ends when release is called (which
// wakes the worker) or after AckHoldLimit. Holds live only in memory, so a
// restart cannot leave a capture blocked.
func (w *Worker) HoldAck(c capture.Capture) (release func()) {
	h := &ackHold{key: keyOf(c), until: w.clock.Now().Add(AckHoldLimit)}
	w.holdsMu.Lock()
	w.holds[h] = struct{}{}
	w.holdsMu.Unlock()

	return func() {
		w.holdsMu.Lock()
		delete(w.holds, h)
		w.holdsMu.Unlock()
		w.Wake()
	}
}

// Alive reports whether Run is executing. It is true from the moment Run
// starts, including while it sleeps on an empty queue or backs off after a
// store error, and false before Run and after it returns. Health reports the
// service unhealthy when it is false ("the worker loop has stopped").
func (w *Worker) Alive() bool { return w.alive.Load() }

// Run processes due captures until ctx is cancelled. It never exits on a
// store or step error: those are retried with backoff (Constitution V). When
// nothing is due it sleeps until the head capture's next attempt or a Wake;
// with an empty queue only Wake (or cancellation) ends the sleep.
func (w *Worker) Run(ctx context.Context) {
	w.alive.Store(true)
	defer w.alive.Store(false)

	for ctx.Err() == nil {
		if !w.sleep(ctx, w.iterate(ctx)) {
			return
		}
	}
}

// iterate does one unit of work and returns how long to wait before the
// next: 0 to continue at once, forever for Wake only.
func (w *Worker) iterate(ctx context.Context) time.Duration {
	if w.pending != nil {
		return w.persist(ctx)
	}

	now := w.clock.Now()
	due, err := w.store.NextDue(ctx, now)
	if err != nil {
		return w.storeFailed(ctx, "next due", err)
	}
	w.failures = 0
	if !due.OK {
		if due.WakeAt.IsZero() {
			return forever
		}

		return max(due.WakeAt.Sub(now), 0)
	}

	c := due.Capture
	if d := w.heldFor(c, now); d > 0 {
		// Its acknowledgement is in flight; release wakes the worker.
		w.log.Debug("waiting for the acknowledgement", zap.Int64("capture", c.ID))

		return d
	}
	if c.QueueReplyEdit && w.notify != nil {
		// The edit was owed but the service stopped before it was sent.
		return w.queueNotice(ctx, c)
	}

	return w.step(ctx, c)
}

// step runs the capture's current step, applies the policy and persists the
// result, then sends the queued notice if the capture just went into waiting.
func (w *Worker) step(ctx context.Context, c capture.Capture) time.Duration {
	next, out := w.run(ctx, c)
	next.ID, next.Step = c.ID, c.Step
	if ctx.Err() != nil {
		// Cancelled mid-step: the result may be half-applied. Drop it; the
		// capture is still at its persisted step and is retried after restart.
		return 0
	}
	res := w.policy.Next(next, out, w.clock.Now())
	w.log.Debug("step finished",
		zap.Int64("capture", c.ID), zap.Stringer("step", c.Step),
		zap.Stringer("outcome", out.Class), zap.String("reason", out.Reason))
	w.pending = &res
	if d := w.persist(ctx); d != 0 || !res.QueueReplyEdit || w.notify == nil {
		return d
	}

	return w.queueNotice(ctx, res)
}

// queueNotice edits the reply of a capture whose waiting state, with
// QueueReplyEdit set, is already persisted, then persists the cleared flag
// so retries and restarts do not edit again. A failed edit is not retried
// (see QueueNotifier), so it can never block or lose the capture.
func (w *Worker) queueNotice(ctx context.Context, c capture.Capture) time.Duration {
	done := w.notify(ctx, c)
	done.ID, done.Step, done.QueueReplyEdit = c.ID, c.Step, false
	if ctx.Err() != nil {
		return 0
	}
	w.pending = &done

	return w.persist(ctx)
}

// run invokes the handler for c's step. A step without a handler (an
// unknown value read from the store) is a programming error: it is logged
// and treated as a temporary failure, so the capture waits with backoff
// instead of being dropped.
func (w *Worker) run(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	h, ok := w.handlers[c.Step]
	if !ok {
		w.log.Error("no handler for step", zap.Int64("capture", c.ID), zap.Stringer("step", c.Step))

		return c, capture.Temporary(fmt.Sprintf("pipeline: no handler for step %q", c.Step))
	}

	return h(ctx, c)
}

// persist writes the pending result. On failure it keeps the result and
// asks for a backoff before retrying.
func (w *Worker) persist(ctx context.Context) time.Duration {
	if err := w.store.Update(ctx, *w.pending); err != nil {
		return w.storeFailed(ctx, "update", err)
	}
	w.pending = nil
	w.failures = 0

	return 0
}

func (w *Worker) storeFailed(ctx context.Context, op string, err error) time.Duration {
	if ctx.Err() != nil {
		return 0
	}
	w.failures++
	d := capture.Backoff(w.failures, w.policy.MaxInterval, w.policy.Rand)
	w.log.Warn("store error, retrying",
		zap.String("op", op), zap.Int("failures", w.failures), zap.Duration("retry_in", d), zap.Error(err))

	return d
}

// sleep waits for d (forever: until Wake), a Wake or cancellation, and
// reports whether the loop should continue.
func (w *Worker) sleep(ctx context.Context, d time.Duration) bool {
	if d == 0 {
		return ctx.Err() == nil
	}
	var timer <-chan time.Time
	if d != forever {
		timer = w.clock.After(d)
	}
	select {
	case <-ctx.Done():
		return false
	case <-w.wake:
	case <-timer:
	}

	return true
}

// heldFor returns how long c is still held, or 0 when it is free.
func (w *Worker) heldFor(c capture.Capture, now time.Time) time.Duration {
	w.holdsMu.Lock()
	defer w.holdsMu.Unlock()

	var d time.Duration
	key := keyOf(c)
	for h := range w.holds {
		if h.key == key {
			d = max(d, h.until.Sub(now))
		}
	}

	return d
}
