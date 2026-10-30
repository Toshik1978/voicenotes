package health_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/health"
)

var errDown = errors.New("fake: down")

type fakeQueue struct {
	count      int
	oldest     time.Time
	waitingOn  string
	misconfig  bool
	has        bool
	countErr   error
	oldestErr  error
	digestDate string
	digestErr  error
}

func (q fakeQueue) CountUnfinished(context.Context) (int, error) { return q.count, q.countErr }

func (q fakeQueue) OldestUnfinished(context.Context) (*health.Oldest, error) {
	if !q.has {
		return nil, q.oldestErr
	}

	return &health.Oldest{ReceivedAt: q.oldest, WaitingOn: q.waitingOn, Misconfig: q.misconfig}, q.oldestErr
}

func (q fakeQueue) LastDigestDate(context.Context) (string, error) { return q.digestDate, q.digestErr }

type fakeWorker struct{ alive, storeOK bool }

func (w fakeWorker) Alive() bool   { return w.alive }
func (w fakeWorker) StoreOK() bool { return w.storeOK }

func TestCollectorSnapshot(t *testing.T) {
	started := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	now := started.Add(time.Hour)
	poll := now.Add(-time.Minute)

	tests := []struct {
		name   string
		queue  fakeQueue
		worker fakeWorker
		poll   time.Time
		want   health.Input
	}{
		{
			name: "busy",
			queue: fakeQueue{
				count:      3,
				oldest:     now.Add(-47 * time.Minute),
				waitingOn:  "stt",
				has:        true,
				digestDate: "2026-10-30",
			},
			worker: fakeWorker{alive: true, storeOK: true},
			poll:   poll,
			want: health.Input{
				Now: now, Started: started, Threshold: 30 * time.Minute,
				Pending: 3, OldestPendingAge: 47 * time.Minute, WaitingOn: "stt",
				LastPollOK: poll, WorkerAlive: true, StoreOK: true, LastDigest: "2026-10-30",
			},
		},
		{
			name: "head waits on a misconfiguration",
			queue: fakeQueue{
				count: 1, oldest: now.Add(-time.Minute), waitingOn: "memos", misconfig: true, has: true,
			},
			worker: fakeWorker{alive: true, storeOK: true},
			poll:   poll,
			want: health.Input{
				Now: now, Started: started, Threshold: 30 * time.Minute,
				Pending: 1, OldestPendingAge: time.Minute, WaitingOn: "memos", Misconfig: true,
				LastPollOK: poll, WorkerAlive: true, StoreOK: true,
			},
		},
		{
			name:   "empty queue and no poll yet",
			worker: fakeWorker{alive: true, storeOK: true},
			want: health.Input{
				Now: now, Started: started, Threshold: 30 * time.Minute,
				WorkerAlive: true, StoreOK: true,
			},
		},
		{
			name:   "worker reports a dead store",
			worker: fakeWorker{alive: true},
			want: health.Input{
				Now: now, Started: started, Threshold: 30 * time.Minute, WorkerAlive: true,
			},
		},
		{
			name:   "count query fails",
			queue:  fakeQueue{countErr: errDown},
			worker: fakeWorker{alive: true, storeOK: true},
			want: health.Input{
				Now: now, Started: started, Threshold: 30 * time.Minute, WorkerAlive: true,
			},
		},
		{
			name:   "oldest query fails",
			queue:  fakeQueue{oldestErr: errDown},
			worker: fakeWorker{alive: true, storeOK: true},
			want: health.Input{
				Now: now, Started: started, Threshold: 30 * time.Minute, WorkerAlive: true,
			},
		},
		{
			name:   "digest lookup failure is not a store failure",
			queue:  fakeQueue{digestErr: errDown},
			worker: fakeWorker{alive: true, storeOK: true},
			want: health.Input{
				Now: now, Started: started, Threshold: 30 * time.Minute, WorkerAlive: true, StoreOK: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := health.NewCollector(health.CollectorConfig{
				Queue: tt.queue, Worker: tt.worker,
				LastPoll:  func() time.Time { return tt.poll },
				Clock:     func() time.Time { return now },
				Started:   started,
				Threshold: 30 * time.Minute,
			})
			if got := c.Snapshot(context.Background()); got != tt.want {
				t.Errorf("snapshot = %+v\nwant       %+v", got, tt.want)
			}
		})
	}
}

func TestFreshProcessIsNotDegraded(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 5, 0, time.UTC)
	c := health.NewCollector(health.CollectorConfig{
		Queue: fakeQueue{}, Worker: fakeWorker{alive: true, storeOK: true},
		LastPoll: func() time.Time { return time.Time{} },
		Clock:    func() time.Time { return now },
		Started:  now.Add(-5 * time.Second), Threshold: 30 * time.Minute,
	})
	if got := health.Compute(c.Snapshot(context.Background())).Status; got != health.StatusOK {
		t.Errorf("status = %s, want ok", got)
	}
}
