package micapi_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/micapi"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

const notifyChat = int64(-1001234567890)

type fakeStore struct {
	mu      sync.Mutex
	rows    map[int64][2]int64
	puts    int
	putErr  error
	takeErr error
	hasErr  error
	// recorded holds the IDs of ingested recordings.
	recorded map[int64]bool
	recErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[int64][2]int64{}, recorded: map[int64]bool{}}
}

func (f *fakeStore) HasRecording(_ context.Context, chat, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if chat != notifyChat {
		return false, nil
	}

	return f.recorded[id], f.recErr
}

func (f *fakeStore) PutStartReply(_ context.Context, id, chat, msg int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.puts++
	if _, ok := f.rows[id]; !ok {
		f.rows[id] = [2]int64{chat, msg}
	}

	return nil
}

func (f *fakeStore) TakeStartReply( //nolint:revive // the four results are the specified signature
	_ context.Context, id int64,
) (int64, int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.takeErr != nil {
		return 0, 0, false, f.takeErr
	}
	r, ok := f.rows[id]
	delete(f.rows, id)

	return r[0], r[1], ok, nil
}

func (f *fakeStore) HasStartReply(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[id]

	return ok, f.hasErr
}

type sent struct {
	chat, replyTo int64
	text          string
}

type fakeReplier struct {
	mu       sync.Mutex
	sends    []sent
	deletes  [][2]int64
	sendErr  error
	delErr   error
	nextID   int64
	block    chan struct{} // when set, Send waits on it
	entered  chan struct{}
	deadline bool
}

func (f *fakeReplier) Send(ctx context.Context, chat, replyTo int64, text string) (int64, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, f.deadline = ctx.Deadline()
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sends = append(f.sends, sent{chat, replyTo, text})
	f.nextID++

	return 100 + f.nextID, nil
}

func (f *fakeReplier) Edit(context.Context, int64, int64, string) (int64, error) {
	return 0, errors.New("unexpected edit")
}

func (f *fakeReplier) Delete(_ context.Context, chat, msg int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, [2]int64{chat, msg})

	return f.delErr
}

var _ pipeline.Replier = (*fakeReplier)(nil)

func TestStartedSendsAndStores(t *testing.T) {
	st, rp := newFakeStore(), &fakeReplier{}
	s := micapi.NewStarts(st, rp, notifyChat)

	if err := s.Started(context.Background(), 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(rp.sends) != 1 || rp.sends[0] != (sent{notifyChat, 0, "🎙 recording…"}) {
		t.Fatalf("sends = %+v", rp.sends)
	}
	if !rp.deadline {
		t.Error("send had no timeout")
	}
	if got := st.rows[7]; got != [2]int64{notifyChat, 101} {
		t.Errorf("row = %v", got)
	}
}

func TestStartedRepeatIsNoop(t *testing.T) {
	st, rp := newFakeStore(), &fakeReplier{}
	s := micapi.NewStarts(st, rp, notifyChat)
	for range 2 {
		if err := s.Started(context.Background(), 7, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if len(rp.sends) != 1 || st.puts != 1 {
		t.Fatalf("sends=%d puts=%d, want 1 and 1", len(rp.sends), st.puts)
	}
}

func TestStartedConcurrentDuplicateSendsOnce(t *testing.T) {
	st := newFakeStore()
	rp := &fakeReplier{block: make(chan struct{}), entered: make(chan struct{}, 4)}
	s := micapi.NewStarts(st, rp, notifyChat)

	done := make(chan error, 1)
	go func() { done <- s.Started(context.Background(), 7, time.Now()) }()
	<-rp.entered // first call is inside Send

	if err := s.Started(context.Background(), 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	close(rp.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(rp.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(rp.sends))
	}
}

func TestStartedErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		st   func(*fakeStore)
		rp   func(*fakeReplier)
	}{
		{"send", func(*fakeStore) {}, func(r *fakeReplier) { r.sendErr = boom }},
		{"put", func(s *fakeStore) { s.putErr = boom }, func(*fakeReplier) {}},
		{"has", func(s *fakeStore) { s.hasErr = boom }, func(*fakeReplier) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, rp := newFakeStore(), &fakeReplier{}
			tt.st(st)
			tt.rp(rp)
			s := micapi.NewStarts(st, rp, notifyChat)
			if err := s.Started(context.Background(), 7, time.Now()); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want boom", err)
			}
			// A failure must not block a retry of the same recording.
			st.putErr, st.hasErr, rp.sendErr = nil, nil, nil
			if err := s.Started(context.Background(), 7, time.Now()); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestStartedSendTimeout(t *testing.T) {
	st := newFakeStore()
	rp := &fakeReplier{block: make(chan struct{})}
	s := micapi.NewStarts(st, rp, notifyChat)
	s.SetTimeout(20 * time.Millisecond)

	if err := s.Started(context.Background(), 7, time.Now()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if len(st.rows) != 0 {
		t.Error("row stored despite failed send")
	}
}

func TestDiscarded(t *testing.T) {
	st, rp := newFakeStore(), &fakeReplier{}
	st.rows[7] = [2]int64{notifyChat, 555}
	s := micapi.NewStarts(st, rp, notifyChat)

	if err := s.Discarded(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if len(rp.deletes) != 1 || rp.deletes[0] != [2]int64{notifyChat, 555} {
		t.Fatalf("deletes = %v", rp.deletes)
	}
	if _, ok := st.rows[7]; ok {
		t.Error("row not removed")
	}
}

func TestDiscardedMissingRowIsNoop(t *testing.T) {
	st, rp := newFakeStore(), &fakeReplier{}
	s := micapi.NewStarts(st, rp, notifyChat)
	if err := s.Discarded(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if len(rp.deletes) != 0 {
		t.Fatalf("deletes = %v", rp.deletes)
	}
}

func TestDiscardedDeleteErrors(t *testing.T) {
	boom := errors.New("boom")
	st, rp := newFakeStore(), &fakeReplier{delErr: pipeline.ErrRejected}
	st.rows[7] = [2]int64{notifyChat, 555}
	s := micapi.NewStarts(st, rp, notifyChat)
	if err := s.Discarded(context.Background(), 7); err != nil {
		t.Fatalf("rejected delete should be ignored, got %v", err)
	}

	st.rows[8] = [2]int64{notifyChat, 556}
	rp.delErr = boom
	if err := s.Discarded(context.Background(), 8); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}

	st.takeErr = boom
	if err := s.Discarded(context.Background(), 9); !errors.Is(err, boom) {
		t.Fatalf("take err = %v, want boom", err)
	}
}

func TestDiscardedBeforeStartedSkipsSend(t *testing.T) {
	st, rp := newFakeStore(), &fakeReplier{}
	s := micapi.NewStarts(st, rp, notifyChat)

	if err := s.Discarded(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if err := s.Started(context.Background(), 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(rp.sends) != 0 || len(st.rows) != 0 {
		t.Fatalf("sends=%d rows=%v, want none", len(rp.sends), st.rows)
	}
}

func TestDiscardedDuringStartedDeletesSentMessage(t *testing.T) {
	st := newFakeStore()
	rp := &fakeReplier{block: make(chan struct{}), entered: make(chan struct{}, 4)}
	s := micapi.NewStarts(st, rp, notifyChat)

	done := make(chan error, 1)
	go func() { done <- s.Started(context.Background(), 7, time.Now()) }()
	<-rp.entered // the start notice is inside Send

	if err := s.Discarded(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	close(rp.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(rp.sends) != 1 || len(rp.deletes) != 1 || rp.deletes[0] != [2]int64{notifyChat, 101} {
		t.Fatalf("sends=%v deletes=%v", rp.sends, rp.deletes)
	}
	if len(st.rows) != 0 {
		t.Errorf("row kept: %v", st.rows)
	}
}

func TestDiscardIsForgottenAfterTTL(t *testing.T) {
	st, rp := newFakeStore(), &fakeReplier{}
	s := micapi.NewStarts(st, rp, notifyChat)
	now := time.Now()
	s.SetClock(func() time.Time { return now })

	if err := s.Discarded(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if err := s.Discarded(context.Background(), 8); err != nil { // prunes 7
		t.Fatal(err)
	}
	if err := s.Started(context.Background(), 7, now); err != nil {
		t.Fatal(err)
	}
	if len(rp.sends) != 1 {
		t.Fatalf("sends = %d, want 1 after the tombstone expired", len(rp.sends))
	}
}

func TestStartedAfterIngestDeletesMessage(t *testing.T) {
	st, rp := newFakeStore(), &fakeReplier{}
	st.recorded[7] = true
	s := micapi.NewStarts(st, rp, notifyChat)

	if err := s.Started(context.Background(), 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(rp.sends) != 1 || len(rp.deletes) != 1 || rp.deletes[0] != [2]int64{notifyChat, 101} {
		t.Fatalf("sends=%v deletes=%v", rp.sends, rp.deletes)
	}
	if len(st.rows) != 0 {
		t.Errorf("row kept: %v", st.rows)
	}
}

func TestStartedRecordingLookupError(t *testing.T) {
	boom := errors.New("boom")
	st, rp := newFakeStore(), &fakeReplier{}
	st.recErr = boom
	s := micapi.NewStarts(st, rp, notifyChat)
	if err := s.Started(context.Background(), 7, time.Now()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}
