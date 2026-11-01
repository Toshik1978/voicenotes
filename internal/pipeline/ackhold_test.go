package pipeline_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// lockedReplier is a goroutine-safe Replier: the acknowledgement runs on the
// test goroutine while the worker runs finalize_reply. failNext fails the
// next Send.
type lockedReplier struct {
	mu       sync.Mutex
	nextID   int64
	failNext bool
	sends    []string
	edits    []sendCall
}

func (r *lockedReplier) Send(_ context.Context, _, _ int64, text string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.failNext {
		r.failNext = false
		return 0, errNet
	}
	r.nextID++
	r.sends = append(r.sends, text)

	return r.nextID, nil
}

func (r *lockedReplier) Edit(_ context.Context, chat, msg int64, text string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.edits = append(r.edits, sendCall{chat: chat, msg: msg, text: text})

	return msg, nil
}

func (r *lockedReplier) result() (sends []string, edits []sendCall) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.sends), slices.Clone(r.edits)
}

// queueInserter inserts into the worker's fake store, so the real Ingester
// and Worker share one queue.
type queueInserter struct {
	s      *fakeStore
	nextID int64
}

func (q *queueInserter) Insert(_ context.Context, c *capture.Capture) error {
	q.nextID++
	c.ID = q.nextID
	q.s.Insert(*c)

	return nil
}

// setReply mirrors sqlite SetReplyMessageID: it keeps a recorded reply.
func (s *fakeStore) setReply(id, msgID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := s.rows[id]
	if c.ReplyMessageID == 0 {
		c.ReplyMessageID = msgID
	}
	s.rows[id] = c
}

// ackRig runs a real Ingester and Worker with the real step handlers (LLM
// off) over one fake queue, which is the US1 baseline the race hits.
type ackRig struct {
	store     *fakeStore
	clock     *fakeClock
	rep       *lockedReplier
	worker    *pipeline.Worker
	in        *pipeline.Ingester
	finalized chan capture.Capture
}

func newAckRig(t *testing.T) *ackRig {
	t.Helper()

	r := &ackRig{
		store:     newFakeStore(),
		clock:     newFakeClock(t0()),
		rep:       &lockedReplier{nextID: 9000},
		finalized: make(chan capture.Capture, 10),
	}
	hs, err := pipeline.NewHandlers(pipeline.Deps{
		Transcriber: &fakeTranscriber{out: capture.OK()}, Notes: &fakeNotes{}, Replier: r.rep,
		Downloader: &fakeDownloader{}, Files: newFakeAudio(),
	}, pipeline.StepsConfig{InboxTag: "inbox", TodoTag: "todo", NoteLimit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	finalize := hs[capture.StepFinalizeReply]
	hs[capture.StepFinalizeReply] = func(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
		r.finalized <- c
		return finalize(ctx, c)
	}
	r.worker = newWorker(t, r.store, r.clock, hs)
	r.in, err = pipeline.NewIngester(&queueInserter{s: r.store}, r.worker)
	if err != nil {
		t.Fatal(err)
	}
	start(t, r.worker)
	r.store.awaitIdle(t)

	return r
}

func telegramText(t *testing.T) capture.Capture {
	t.Helper()

	c := textCapture(t, 0)
	c.Source, c.SourceChatID, c.SourceMessageID, c.Text = "telegram", -1001234567890, 42, "hello"

	return c
}

// awaitHeldOrFinalized wakes the worker as another capture's ingest would,
// then blocks until the worker either holds the capture (it sleeps on the
// clock) or has already reached finalize_reply (the race). It reports which.
func (r *ackRig) awaitHeldOrFinalized(t *testing.T) (held time.Duration, raced bool) {
	t.Helper()

	r.worker.Wake()
	select {
	case d := <-r.clock.waits:
		return d, false
	case c := <-r.finalized:
		r.finalized <- c
		return 0, true
	case <-time.After(5 * time.Second):
		t.Fatal("worker neither held the capture nor finalized it")
		return 0, false
	}
}

func (r *ackRig) awaitDone(t *testing.T, id int64) {
	t.Helper()

	for {
		if c := r.store.awaitUpdate(t); c.ID == id && c.Status == capture.StatusDone {
			return
		}
	}
}

// FR-025, US1 AS1/AS5: the worker reaches finalize_reply while the ⏳ send
// is still in flight. The ⏳ must be the one reply, edited to the result.
func TestWorkerEditsTheAckThatWasInFlight(t *testing.T) {
	r := newAckRig(t)

	stored, err := r.in.Ingest(context.Background(), telegramText(t),
		func(ctx context.Context, c capture.Capture) {
			held, raced := r.awaitHeldOrFinalized(t)
			if raced {
				t.Error("worker reached finalize_reply before the ⏳ was recorded")
			} else if held != pipeline.AckHoldLimit {
				t.Errorf("held for %v, want %v", held, pipeline.AckHoldLimit)
			}
			id, err := r.rep.Send(ctx, c.SourceChatID, c.SourceMessageID, "⏳")
			if err != nil {
				t.Fatal(err)
			}
			r.store.setReply(c.ID, id)
		})
	if err != nil {
		t.Fatal(err)
	}
	r.awaitDone(t, stored.ID)

	sends, edits := r.rep.result()
	if !slices.Equal(sends, []string{"⏳"}) {
		t.Fatalf("sent %q, want only the ⏳", sends)
	}
	if len(edits) != 1 || edits[0].msg != 9001 || !strings.HasPrefix(edits[0].text, "✓ saved") {
		t.Fatalf("edits = %+v, want the ⏳ (9001) edited to ✓ saved", edits)
	}
}

// A failed ⏳ send releases the capture at once, and finalize_reply falls
// back to exactly one new reply.
func TestWorkerSendsOneReplyWhenTheAckFailed(t *testing.T) {
	r := newAckRig(t)

	stored, err := r.in.Ingest(context.Background(), telegramText(t),
		func(ctx context.Context, c capture.Capture) {
			if _, raced := r.awaitHeldOrFinalized(t); raced {
				t.Error("worker reached finalize_reply during the acknowledgement")
			}
			r.rep.mu.Lock()
			r.rep.failNext = true
			r.rep.mu.Unlock()
			if _, err := r.rep.Send(ctx, c.SourceChatID, c.SourceMessageID, "⏳"); !errors.Is(err, errNet) {
				t.Errorf("⏳ send: %v, want failure", err)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	r.awaitDone(t, stored.ID)

	sends, edits := r.rep.result()
	if len(sends) != 1 || !strings.HasPrefix(sends[0], "✓ saved") || len(edits) != 0 {
		t.Fatalf("sends %q, edits %+v: want exactly one new ✓ saved reply", sends, edits)
	}
}

// An acknowledgement that never finishes cannot block the queue: after
// AckHoldLimit the worker goes on and finalize_reply sends a new reply.
func TestWorkerHoldIsBounded(t *testing.T) {
	r := newAckRig(t)

	stored, err := r.in.Ingest(context.Background(), telegramText(t),
		func(context.Context, capture.Capture) {
			if _, raced := r.awaitHeldOrFinalized(t); raced {
				t.Error("worker reached finalize_reply during the acknowledgement")
				return
			}
			r.clock.Advance(pipeline.AckHoldLimit)
			select {
			case <-r.finalized:
			case <-time.After(5 * time.Second):
				t.Error("worker stayed blocked after the hold limit")
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	r.awaitDone(t, stored.ID)

	sends, _ := r.rep.result()
	if len(sends) != 1 || !strings.HasPrefix(sends[0], "✓ saved") {
		t.Fatalf("sends %q, want one new ✓ saved reply", sends)
	}
}

// Release wakes the worker only when it actually deferred the held capture:
// otherwise nothing waits on the hold, and a file item that joined a batch
// without moving its deadline must not wake it.
func TestHoldReleaseWakesOnlyWhenTheWorkerDeferred(t *testing.T) {
	c := telegramText(t)
	for name, deferred := range map[string]bool{"deferred": true, "not deferred": false} {
		t.Run(name, func(t *testing.T) {
			clock := newFakeClock(t0())
			w := newWorker(
				t,
				newFakeStore(),
				clock,
				allSteps(func(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
					return c, capture.OK()
				}),
			)
			release := w.HoldAck(c)
			if deferred {
				if d := w.HeldFor(c, clock.Now()); d != pipeline.AckHoldLimit {
					t.Fatalf("held for %v, want %v", d, pipeline.AckHoldLimit)
				}
			}
			release()
			if got := w.WakePending(); got != deferred {
				t.Fatalf("wake pending = %v, want %v", got, deferred)
			}
		})
	}
}
