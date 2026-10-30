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

func runPurge(t *testing.T, p *fakePurger, now time.Time) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		pipeline.PurgeLoop(ctx, p, 5*time.Millisecond, func() time.Time { return now }, nil)
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
