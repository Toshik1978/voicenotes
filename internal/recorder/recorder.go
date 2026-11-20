package recorder

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/mic"
)

const (
	// rediscoverInterval paces discovery while no receiver is present (FR-206).
	rediscoverInterval = 5 * time.Second
	// tickInterval paces the maximum-length check.
	tickInterval = time.Second
	// defaultFrameBuffer is the frame queue's capacity in audio callbacks: at
	// miniaudio's ~10 ms period, several seconds of slack for a slow fsync.
	defaultFrameBuffer = 1024
)

// ErrDeviceLost is what an [AudioStream] reports when capture stops because
// the device went away.
var ErrDeviceLost = errors.New("recorder: audio device lost")

// KeyEvent is one event from the receiver's key input.
type KeyEvent int

// The key events.
const (
	// KeyTap is a "volume up" press.
	KeyTap KeyEvent = iota + 1
	// KeyLost means the input device went away; no event follows it.
	KeyLost
)

// KeyStream is an open key input.
type KeyStream struct {
	// Events delivers taps. A KeyLost event, or closing the channel, ends
	// the stream.
	Events <-chan KeyEvent
	// Close releases the device.
	Close func() error
	// Grabbed reports whether the input is held exclusively (FR-202).
	Grabbed bool
}

// KeySource opens the receiver's key input (micdev.Keys).
type KeySource interface {
	// Open opens the input device at path and tries to grab it.
	Open(path string) (KeyStream, error)
}

// AudioStream is a running capture.
type AudioStream struct {
	// Lost receives once, an error wrapping ErrDeviceLost, if capture stops
	// on its own. It may be nil.
	Lost <-chan error
	// Close stops capture. No frame is offered after it returns.
	Close func() error
}

// AudioSource captures s16le 16 kHz mono audio (micaudio.Capture).
type AudioSource interface {
	// Open starts capturing from the receiver's card (or the explicitly
	// configured device) into q, offering frames without ever blocking.
	Open(rcv mic.Receiver, q *FrameQueue) (AudioStream, error)
}

// Discovery finds the receiver (micdev.Discover). An explicit device
// configuration is the implementation's business.
type Discovery interface {
	// Discover reports the receiver, or false when none is connected.
	Discover() (mic.Receiver, bool, error)
}

// Spool is the recorder's durable store of recordings; [DirSpool] is the
// implementation.
type Spool interface {
	Recover() (Recovery, error)
	// Begin returns the audio writer; it must be closed before Finish.
	Begin(rec Recording) (io.WriteCloser, error)
	Finish(id int64, endedAt time.Time, reason mic.EndReason) error
	Discard(id int64) error
	// Finished lists finished recordings, oldest first.
	Finished() ([]Recording, error)
	Open(id int64) (io.ReadSeekCloser, int64, error)
	Remove(id int64) error
	FreeBytes() (int64, error)
}

var _ Spool = (*DirSpool)(nil)

// Result is the outcome of one hand-off attempt (contract "Client retry
// policy").
type Result int

// The hand-off results.
const (
	// Stored means the service stored a new capture (201).
	Stored Result = iota + 1
	// Duplicate means the service already had it (200).
	Duplicate
	// Retry means a transient failure: corrupt body, 5xx, network.
	Retry
	// Misconfigured means the credential was refused (401).
	Misconfigured
	// Rejected means the service refused it for good (413, 415, other 400).
	Rejected
)

// Handoff uploads a finished recording (recorder.HTTPHandoff).
type Handoff interface {
	// Upload sends size bytes of body. The error, if any, explains a result
	// other than Stored or Duplicate; the result alone decides what happens.
	Upload(ctx context.Context, rec Recording, body io.ReadSeeker, size int64) (Result, error)
}

// Notifier sends the best-effort start and discard notices.
type Notifier interface {
	Started(ctx context.Context, id int64, startedAt time.Time) error
	Discarded(ctx context.Context, id int64) error
}

// CheckIn reports the recorder's status to the service.
type CheckIn interface {
	Send(ctx context.Context, s Status) error
}

// Clock is the recorder's time source.
type Clock interface {
	Now() time.Time
	// After is like time.After.
	After(d time.Duration) <-chan time.Time
	// NewTicker is like time.NewTicker; the func stops it.
	NewTicker(d time.Duration) (<-chan time.Time, func())
}

// Status is a snapshot of the recorder for /healthz and check-ins.
type Status struct {
	// OldestPendingAt is the start of the oldest recording awaiting
	// hand-off; zero when none is.
	OldestPendingAt time.Time
	// Pending counts recordings awaiting hand-off, rejected ones excluded.
	Pending int
	// Rejected counts recordings the service refused for good.
	Rejected        int
	ReceiverPresent bool
	InputGrabbed    bool
	Recording       bool
	StorageFull     bool
	// Misconfigured is set by a 401 and cleared by any later answer that
	// proves the credential.
	Misconfigured bool
	// SpoolError is set while the spool cannot be listed.
	SpoolError bool
	// Running is true while Run's loops are up.
	Running bool
}

// Deps are the recorder's collaborators. All but Log are required.
type Deps struct {
	Keys      KeySource
	Audio     AudioSource
	Discovery Discovery
	Spool     Spool
	Handoff   Handoff
	Notifier  Notifier
	CheckIn   CheckIn
	// Log defaults to a no-op logger.
	Log *zap.Logger
}

// Options are the recorder's settings (config.RecorderConfig) and test seams.
type Options struct {
	Clock            Clock          // nil: the system clock
	NewID            func() int64   // nil: mic.NewID
	Rand             func() float64 // backoff jitter; nil: math/rand/v2
	Recorder         string
	Debounce         time.Duration
	MinLength        time.Duration
	MaxLength        time.Duration
	CheckinInterval  time.Duration
	RetryMaxInterval time.Duration
	SpoolMinFree     int64
	FrameBuffer      int // frame queue capacity; 0: default
}

// FrameQueue is the bounded channel between the audio callback and the
// spool writer.
type FrameQueue struct {
	ch      chan []byte
	dropped atomic.Uint64
}

// NewFrameQueue returns a queue holding up to n chunks.
func NewFrameQueue(n int) *FrameQueue {
	return &FrameQueue{ch: make(chan []byte, n)}
}

// Offer queues a chunk of frames without blocking and takes ownership of b.
// A full queue drops the chunk, counts its frames and returns false. It is
// safe to call from the audio callback.
func (q *FrameQueue) Offer(b []byte) bool {
	select {
	case q.ch <- b:
		return true
	default:
		q.dropped.Add(uint64(len(b) / mic.FrameSize))
		return false
	}
}

// Dropped returns the number of frames dropped so far.
func (q *FrameQueue) Dropped() uint64 { return q.dropped.Load() }

// Recorder is the recording loop: discovery, taps through mic.Toggle, the
// spool, and the hand-off to the service.
type Recorder struct {
	d      Deps
	o      Options
	log    *zap.Logger
	toggle *mic.Toggle
	wake   chan struct{}
	notes  sync.WaitGroup

	// Owned by the event loop.
	active *activeRec

	// Owned by the hand-off loop.
	rejected     map[int64]bool
	attempts     int
	listFailures int

	mu     sync.Mutex
	status Status
}

// New validates the setup and returns an idle recorder.
func New(d Deps, o Options) (*Recorder, error) {
	if err := validate(d, o); err != nil {
		return nil, err
	}

	d, o = withDefaults(d, o)

	return &Recorder{
		d:        d,
		o:        o,
		log:      d.Log,
		toggle:   mic.NewToggle(o.Debounce, o.MinLength, o.MaxLength, o.NewID),
		wake:     make(chan struct{}, 1),
		rejected: map[int64]bool{},
	}, nil
}

func validate(d Deps, o Options) error {
	for _, dep := range []any{d.Keys, d.Audio, d.Discovery, d.Spool, d.Handoff, d.Notifier, d.CheckIn} {
		if dep == nil {
			return errors.New("recorder: a dependency is missing")
		}
	}

	if o.Recorder == "" || o.MaxLength <= 0 || o.CheckinInterval <= 0 || o.RetryMaxInterval <= 0 {
		return errors.New("recorder: recorder name, maximum length and intervals are required")
	}

	return nil
}

func withDefaults(d Deps, o Options) (Deps, Options) {
	if d.Log == nil {
		d.Log = zap.NewNop()
	}

	if o.Clock == nil {
		o.Clock = systemClock{}
	}

	if o.NewID == nil {
		o.NewID = mic.NewID
	}

	if o.Rand == nil {
		o.Rand = rand.Float64
	}

	if o.FrameBuffer <= 0 {
		o.FrameBuffer = defaultFrameBuffer
	}

	return d, o
}

// Status returns a snapshot; it is safe for concurrent use.
func (r *Recorder) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.status
}

// Run recovers the spool, then runs discovery and recording, the hand-off
// and the check-ins until ctx ends. A recording in progress is finished as
// interrupted. Failures degrade and retry; Run returns nil once stopped.
// Call it once.
func (r *Recorder) Run(ctx context.Context) error {
	r.recover()
	r.setStatus(func(s *Status) { s.Running = true })
	r.refreshStorage()

	var loops sync.WaitGroup

	loops.Go(func() { r.handoffLoop(ctx) })
	loops.Go(func() { r.checkInLoop(ctx) })
	r.deviceLoop(ctx)
	loops.Wait()
	r.notes.Wait()
	r.setStatus(func(s *Status) { s.Running = false })

	return nil
}

func (r *Recorder) recover() {
	res, err := r.d.Spool.Recover()
	if err != nil {
		r.log.Error("spool recovery incomplete", zap.Error(err))
	}

	if len(res.Recovered) > 0 || len(res.Broken) > 0 {
		r.log.Warn("spool recovered after an interruption",
			zap.Int64s("recovered", res.Recovered), zap.Strings("broken", res.Broken))
	}
}

func (r *Recorder) setStatus(f func(s *Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.status)
}

// refreshStorage checks free space against MIC_SPOOL_MIN_FREE and reports
// whether storage is full. Unknown free space counts as full.
func (r *Recorder) refreshStorage() bool {
	free, err := r.d.Spool.FreeBytes()
	if err != nil {
		r.log.Warn("spool free space unknown", zap.Error(err))
	}

	full := err != nil || free < r.o.SpoolMinFree
	r.setStatus(func(s *Status) { s.StorageFull = full })

	return full
}

// sleep waits for d or cancellation and reports whether to go on.
func (r *Recorder) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}

	select {
	case <-ctx.Done():
		return false
	case <-r.o.Clock.After(d):
		return true
	}
}

// ---------------------------------------------------------------- devices

func (r *Recorder) deviceLoop(ctx context.Context) {
	present := true // log the first absence

	for ctx.Err() == nil {
		rcv, ok, err := r.d.Discovery.Discover()
		if err != nil {
			r.log.Warn("receiver discovery failed", zap.Error(err))
		}

		switch {
		case ok:
			present = true

			r.session(ctx, rcv)
		case present:
			present = false

			r.log.Warn("no receiver found, rediscovering", zap.Duration("every", rediscoverInterval))
		}

		if !r.sleep(ctx, rediscoverInterval) {
			return
		}
	}
}

// session runs one receiver until it is lost or ctx ends.
func (r *Recorder) session(ctx context.Context, rcv mic.Receiver) {
	keys, err := r.d.Keys.Open(rcv.InputPath)
	if err != nil {
		r.log.Error("open receiver input", zap.String("input", rcv.InputPath), zap.Error(err))
		return
	}

	if !keys.Grabbed {
		r.log.Warn("receiver input not held exclusively", zap.String("input", rcv.InputPath))
	}

	r.log.Info("receiver ready", zap.Int("card", rcv.Card), zap.String("input", rcv.InputPath))
	r.setStatus(func(s *Status) { s.ReceiverPresent, s.InputGrabbed = true, keys.Grabbed })

	tick, stop := r.o.Clock.NewTicker(tickInterval)

	for r.step(ctx, rcv, keys.Events, tick) {
	}

	stop()

	if err := keys.Close(); err != nil {
		r.log.Warn("close receiver input", zap.Error(err))
	}

	r.setStatus(func(s *Status) { s.ReceiverPresent, s.InputGrabbed = false, false })
}

// step handles one event and reports whether the session goes on.
func (r *Recorder) step(ctx context.Context, rcv mic.Receiver, events <-chan KeyEvent, tick <-chan time.Time) bool {
	var (
		lost   <-chan error
		failed <-chan struct{}
	)

	if a := r.active; a != nil {
		lost, failed = a.stream.Lost, a.failed
	}

	select {
	case <-ctx.Done():
		now := r.o.Clock.Now()
		r.apply(ctx, rcv, r.toggle.Interrupt(now), now)

		return false
	case ev, ok := <-events:
		if !ok || ev == KeyLost {
			r.log.Warn("receiver input lost")
			r.lose(ctx, rcv)

			return false
		}

		r.tap(ctx, rcv)
	case <-tick:
		now := r.o.Clock.Now()
		r.apply(ctx, rcv, r.toggle.Tick(now), now)
	case err := <-lost:
		r.log.Warn("audio capture lost", zap.Error(err))
		r.lose(ctx, rcv)

		return false
	case <-failed:
		now := r.o.Clock.Now()
		r.apply(ctx, rcv, r.toggle.Interrupt(now), now)
	}

	return true
}

func (r *Recorder) lose(ctx context.Context, rcv mic.Receiver) {
	now := r.o.Clock.Now()
	r.apply(ctx, rcv, r.toggle.DeviceLost(now), now)
}

func (r *Recorder) tap(ctx context.Context, rcv mic.Receiver) {
	now := r.o.Clock.Now()

	var why mic.RefuseReason
	if !r.toggle.Recording() && r.refreshStorage() {
		why = mic.RefuseStorageFull
	}

	r.apply(ctx, rcv, r.toggle.Tap(now, why), now)
}

func (r *Recorder) apply(ctx context.Context, rcv mic.Receiver, a mic.Action, now time.Time) {
	switch a.Kind {
	case mic.ActionStart:
		r.start(ctx, rcv, a)
	case mic.ActionStop:
		r.stop(a.ID, now, a.End)
	case mic.ActionDiscard:
		r.discard(ctx, a.ID)
	case mic.ActionRefuse:
		r.log.Warn("tap refused", zap.String("reason", string(a.Refusal)))
	case mic.ActionNone:
	}
}

// ---------------------------------------------------------------- recording

// activeRec is the recording in progress: its capture and writer goroutine.
type activeRec struct {
	w      io.WriteCloser
	q      *FrameQueue
	stream AudioStream
	stop   chan struct{} // closed to make the writer drain and exit
	done   chan struct{} // closed when the writer has exited
	failed chan struct{} // receives once if a write fails
	werr   error         // the first write error; read after done
	id     int64
}

// write drains the queue into the spool until stopped, then drains what is
// left. After a write error it keeps draining and discards.
func (a *activeRec) write() {
	defer close(a.done)

	for {
		select {
		case b := <-a.q.ch:
			a.put(b)
		case <-a.stop:
			for {
				select {
				case b := <-a.q.ch:
					a.put(b)
				default:
					return
				}
			}
		}
	}
}

func (a *activeRec) put(b []byte) {
	if a.werr != nil {
		return
	}

	if _, err := a.w.Write(b); err != nil {
		a.werr = err
		a.failed <- struct{}{}
	}
}

func (r *Recorder) start(ctx context.Context, rcv mic.Receiver, a mic.Action) {
	w, err := r.d.Spool.Begin(Recording{ID: a.ID, Recorder: r.o.Recorder, StartedAt: a.At})
	if err != nil {
		r.abort(a.ID, err)
		return
	}

	q := NewFrameQueue(r.o.FrameBuffer)

	stream, err := r.d.Audio.Open(rcv, q)
	if err != nil {
		err = errors.Join(err, w.Close(), r.d.Spool.Discard(a.ID))
		r.abort(a.ID, err)

		return
	}

	act := &activeRec{
		w: w, q: q, stream: stream, id: a.ID,
		stop: make(chan struct{}), done: make(chan struct{}), failed: make(chan struct{}, 1),
	}
	go act.write()

	r.active = act
	r.setStatus(func(s *Status) { s.Recording = true })
	r.log.Info("recording started", zap.Int64("recording_id", a.ID))

	r.notes.Go(func() {
		if err := r.d.Notifier.Started(ctx, a.ID, a.At); err != nil {
			r.log.Info("start notice not sent", zap.Int64("recording_id", a.ID), zap.Error(err))
		}
	})
}

// abort undoes a start that failed, so the next tap starts afresh.
func (r *Recorder) abort(id int64, err error) {
	r.toggle.Interrupt(r.o.Clock.Now())
	r.log.Error("recording could not start", zap.Int64("recording_id", id), zap.Error(err))
}

func (r *Recorder) stop(id int64, endedAt time.Time, reason mic.EndReason) {
	if err := r.endActive(); err != nil {
		r.log.Error("recording audio may be incomplete", zap.Int64("recording_id", id), zap.Error(err))
	}

	if err := r.d.Spool.Finish(id, endedAt, reason); err != nil {
		r.log.Error("finish recording; recovery completes it at the next start",
			zap.Int64("recording_id", id), zap.Error(err))

		return
	}

	r.log.Info("recording finished", zap.Int64("recording_id", id), zap.String("end_reason", string(reason)))
	r.wakeHandoff()
}

func (r *Recorder) discard(ctx context.Context, id int64) {
	if err := r.endActive(); err != nil {
		r.log.Warn("stop short recording", zap.Int64("recording_id", id), zap.Error(err))
	}

	if err := r.d.Spool.Discard(id); err != nil {
		r.log.Error("discard short recording", zap.Int64("recording_id", id), zap.Error(err))
	}

	r.log.Info("short recording discarded", zap.Int64("recording_id", id))
	r.notes.Go(func() {
		if err := r.d.Notifier.Discarded(ctx, id); err != nil {
			r.log.Info("discard notice not sent", zap.Int64("recording_id", id), zap.Error(err))
		}
	})
}

// endActive stops capture, drains the queue into the spool and closes the
// writer.
func (r *Recorder) endActive() error {
	a := r.active
	if a == nil {
		return nil
	}

	r.active = nil
	serr := a.stream.Close()

	close(a.stop)
	<-a.done

	cerr := a.w.Close()

	if n := a.q.Dropped(); n > 0 {
		r.log.Warn("audio frames dropped", zap.Int64("recording_id", a.id), zap.Uint64("frames", n))
	}

	r.setStatus(func(s *Status) { s.Recording = false })

	return errors.Join(serr, a.werr, cerr)
}

// ---------------------------------------------------------------- hand-off

func (r *Recorder) wakeHandoff() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Recorder) handoffLoop(ctx context.Context) {
	for {
		d, idle := r.handoffOnce(ctx)
		if !idle {
			if !r.sleep(ctx, d) {
				return
			}

			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
	}
}

// handoffOnce uploads the oldest eligible recording. It returns how long to
// wait before the next attempt, or idle when there is nothing to send.
func (r *Recorder) handoffOnce(ctx context.Context) (time.Duration, bool) {
	recs, err := r.d.Spool.Finished()
	if err != nil {
		r.listFailures++
		d := capture.Backoff(r.listFailures, r.o.RetryMaxInterval, r.o.Rand)
		r.log.Error("spool listing failed, hand-off paused", zap.Duration("retry_in", d), zap.Error(err))
		r.setStatus(func(s *Status) { s.SpoolError = true })

		return d, false
	}

	r.listFailures = 0

	next, ok := r.pending(recs)
	if !ok {
		return 0, true
	}

	return r.upload(ctx, next), false
}

// pending publishes the queue's state and returns the oldest recording not
// rejected.
func (r *Recorder) pending(recs []Recording) (Recording, bool) {
	var (
		eligible []Recording
		rejected int
	)

	for _, rec := range recs {
		if r.rejected[rec.ID] {
			rejected++
			continue
		}

		eligible = append(eligible, rec)
	}

	r.setStatus(func(s *Status) {
		s.SpoolError, s.Pending, s.Rejected, s.OldestPendingAt = false, len(eligible), rejected, time.Time{}
		if len(eligible) > 0 {
			s.OldestPendingAt = eligible[0].StartedAt
		}
	})

	if len(eligible) == 0 {
		return Recording{}, false
	}

	return eligible[0], true
}

func (r *Recorder) upload(ctx context.Context, rec Recording) time.Duration {
	body, size, err := r.d.Spool.Open(rec.ID)
	if err != nil {
		return r.retry(ctx, rec, err)
	}

	res, err := r.d.Handoff.Upload(ctx, rec, body, size)
	if cerr := body.Close(); cerr != nil {
		r.log.Warn("close spooled recording", zap.Int64("recording_id", rec.ID), zap.Error(cerr))
	}

	switch res {
	case Stored, Duplicate:
		return r.handedOver(ctx, rec, res)
	case Misconfigured:
		r.setStatus(func(s *Status) { s.Misconfigured = true })
		r.log.Error("upload credential refused; check MIC_UPLOAD_TOKEN",
			zap.Int64("recording_id", rec.ID), zap.Duration("retry_in", r.o.RetryMaxInterval), zap.Error(err))

		return r.o.RetryMaxInterval
	case Rejected:
		r.rejected[rec.ID] = true
		r.attempts = 0
		r.setStatus(func(s *Status) { s.Misconfigured = false })
		r.log.Error("recording rejected by the service; kept and skipped until restart",
			zap.Int64("recording_id", rec.ID), zap.Error(err))

		return 0
	case Retry: // like an unknown result, below
	}

	return r.retry(ctx, rec, err)
}

func (r *Recorder) handedOver(ctx context.Context, rec Recording, res Result) time.Duration {
	r.attempts = 0
	r.setStatus(func(s *Status) { s.Misconfigured = false })

	if err := r.d.Spool.Remove(rec.ID); err != nil {
		return r.retry(ctx, rec, err)
	}

	r.log.Info("recording handed over", zap.Int64("recording_id", rec.ID), zap.Bool("duplicate", res == Duplicate))

	return 0
}

func (r *Recorder) retry(ctx context.Context, rec Recording, err error) time.Duration {
	if ctx.Err() != nil {
		return 0
	}

	r.attempts++
	d := capture.Backoff(r.attempts, r.o.RetryMaxInterval, r.o.Rand)
	r.log.Warn("hand-off failed, retrying",
		zap.Int64("recording_id", rec.ID), zap.Int("attempts", r.attempts), zap.Duration("retry_in", d), zap.Error(err))

	return d
}

// ---------------------------------------------------------------- check-in

func (r *Recorder) checkInLoop(ctx context.Context) {
	tick, stop := r.o.Clock.NewTicker(r.o.CheckinInterval)
	defer stop()

	for {
		r.refreshStorage()

		if err := r.d.CheckIn.Send(ctx, r.Status()); err != nil && ctx.Err() == nil {
			r.log.Warn("check-in failed", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

// systemClock is the real Clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (systemClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}
