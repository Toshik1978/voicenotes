package pipeline_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

type fakePurger struct {
	mu     sync.Mutex
	cutoff []time.Time
	err    error
}

func (f *fakePurger) PurgeDone(_ context.Context, olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoff = append(f.cutoff, olderThan)
	if f.err != nil {
		return 0, f.err
	}

	return 1, nil
}

func (f *fakePurger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.cutoff)
}

func runPurge(t *testing.T, p *fakePurger, now time.Time, loop ...pipeline.Purger) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var target pipeline.Purger = p
	if len(loop) > 0 {
		target = loop[0]
	}
	go func() {
		pipeline.PurgeLoop(ctx, target, 5*time.Millisecond, func() time.Time { return now }, nil)
		close(finished)
	}()
	deadline := time.After(2 * time.Second)
	for p.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d purges", p.count())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-finished
}

func TestPurgeLoopRunsAtStartAndOnTick(t *testing.T) {
	p := &fakePurger{}
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	runPurge(t, p, now)

	if got, want := p.cutoff[0], now.Add(-30*24*time.Hour); !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v", got, want)
	}
	if pipeline.DoneRetention != 30*24*time.Hour {
		t.Errorf("retention = %v, want 30 days", pipeline.DoneRetention)
	}
	if pipeline.PurgeInterval != 24*time.Hour {
		t.Errorf("interval = %v, want 24h", pipeline.PurgeInterval)
	}
}

func TestPurgeLoopSurvivesErrors(t *testing.T) {
	p := &fakePurger{err: errors.New("disk full")}
	runPurge(t, p, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
}

type fakeReplyStore struct {
	stale   []pipeline.StartReply
	listErr error
	takeErr error
	cutoff  time.Time
	taken   []int64
}

func (f *fakeReplyStore) StaleStartReplies(_ context.Context, olderThan time.Time) ([]pipeline.StartReply, error) {
	f.cutoff = olderThan

	return f.stale, f.listErr
}

func (f *fakeReplyStore) TakeStartReply( //nolint:revive // the four results are the specified signature
	_ context.Context, id int64,
) (int64, int64, bool, error) {
	f.taken = append(f.taken, id)

	return 0, 0, true, f.takeErr
}

func TestStartReplyJanitor(t *testing.T) {
	st := &fakeReplyStore{stale: []pipeline.StartReply{
		{RecordingID: 1, ChatID: -1001234567890, MessageID: 10},
		{RecordingID: 2, ChatID: -1001234567890, MessageID: 20},
	}}
	rp := &fakeReplier{}
	cutoff := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

	n, err := pipeline.StartReplyJanitor{Store: st, Replier: rp}.PurgeStartReplies(context.Background(), cutoff)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !st.cutoff.Equal(cutoff) {
		t.Errorf("cutoff = %v", st.cutoff)
	}
	if len(rp.edits) != 2 || rp.edits[0].msg != 10 || rp.edits[1].msg != 20 ||
		rp.edits[0].text != "⚠ recording not received" {
		t.Errorf("edits = %+v", rp.edits)
	}
	if len(st.taken) != 2 || st.taken[0] != 1 || st.taken[1] != 2 {
		t.Errorf("taken = %v", st.taken)
	}
}

func TestStartReplyJanitorEditErrorStillRemovesRow(t *testing.T) {
	st := &fakeReplyStore{stale: []pipeline.StartReply{{RecordingID: 1, ChatID: 5, MessageID: 10}}}
	rp := &fakeReplier{editErr: errors.New("message gone")}

	n, err := pipeline.StartReplyJanitor{Store: st, Replier: rp}.PurgeStartReplies(context.Background(), time.Now())
	if err != nil || n != 1 || len(st.taken) != 1 {
		t.Fatalf("n=%d err=%v taken=%v", n, err, st.taken)
	}
}

func TestStartReplyJanitorStoreErrors(t *testing.T) {
	boom := errors.New("boom")
	j := pipeline.StartReplyJanitor{Store: &fakeReplyStore{listErr: boom}, Replier: &fakeReplier{}}
	if _, err := j.PurgeStartReplies(context.Background(), time.Now()); !errors.Is(err, boom) {
		t.Errorf("list err = %v", err)
	}
	st := &fakeReplyStore{stale: []pipeline.StartReply{{RecordingID: 1}}, takeErr: boom}
	j = pipeline.StartReplyJanitor{Store: st, Replier: &fakeReplier{}}
	if _, err := j.PurgeStartReplies(context.Background(), time.Now()); !errors.Is(err, boom) {
		t.Errorf("take err = %v", err)
	}
}

func TestStartReplyJanitorCancelledEditStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := &fakeReplyStore{stale: []pipeline.StartReply{{RecordingID: 1}}}
	rp := &fakeReplier{editErr: context.Canceled}
	n, err := pipeline.StartReplyJanitor{Store: st, Replier: rp}.PurgeStartReplies(ctx, time.Now())
	if !errors.Is(err, context.Canceled) || n != 0 || len(st.taken) != 0 {
		t.Fatalf("n=%d err=%v taken=%v", n, err, st.taken)
	}
}

type replyingPurger struct {
	fakePurger

	cutoffs []time.Time
	err     error
}

func (r *replyingPurger) PurgeStartReplies(_ context.Context, olderThan time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cutoffs = append(r.cutoffs, olderThan)

	return 1, r.err
}

func TestPurgeLoopPurgesStartReplies(t *testing.T) {
	p := &replyingPurger{}
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	runPurge(t, &p.fakePurger, now, p)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cutoffs) == 0 {
		t.Fatal("start replies never purged")
	}
	if got, want := p.cutoffs[0], now.Add(-24*time.Hour); !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v", got, want)
	}
	if pipeline.StartReplyRetention != 24*time.Hour {
		t.Errorf("retention = %v", pipeline.StartReplyRetention)
	}
}

func TestPurgeLoopSurvivesStartReplyErrors(t *testing.T) {
	p := &replyingPurger{err: errors.New("disk full")}
	runPurge(t, &p.fakePurger, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC), p)
}
