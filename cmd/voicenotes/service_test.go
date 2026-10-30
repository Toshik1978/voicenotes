package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	"github.com/Toshik1978/voicenotes/internal/config"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.events...)
}

func TestServeClosesStoreAfterEverythingReturns(t *testing.T) {
	rec := &recorder{}
	slow := func(name string) func(context.Context) {
		return func(ctx context.Context) {
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond) // a drain in progress
			rec.add(name + " stopped")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, []func(context.Context){slow("telegram"), slow("worker"), slow("purge")},
			func() error { rec.add("db closed"); return nil })
	}()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}

	ev := rec.list()
	if len(ev) != 4 || ev[3] != "db closed" {
		t.Fatalf("events = %v, want db closed last after 3 stops", ev)
	}
}

func TestServeReportsCloseError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := serve(ctx, nil, func() error { return errors.New("boom") })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want close error", err)
	}
}

type fakePurger struct {
	mu     sync.Mutex
	cutoff []time.Time
}

func (f *fakePurger) PurgeDone(_ context.Context, olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoff = append(f.cutoff, olderThan)

	return 1, nil
}

func (f *fakePurger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.cutoff)
}

func TestPurgeLoopRunsAtStartAndOnTick(t *testing.T) {
	p := &fakePurger{}
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		purgeLoop(ctx, p, 5*time.Millisecond, func() time.Time { return now }, nil)
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

	want := now.Add(-doneRetention)
	if got := p.cutoff[0]; !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v", got, want)
	}
	if doneRetention != 30*24*time.Hour {
		t.Errorf("retention = %v, want 30 days", doneRetention)
	}
}

func TestRunServiceInvalidConfig(t *testing.T) {
	var stderr bytes.Buffer
	load := func() (config.Config, error) { return config.Parse(map[string]string{}) }
	code := runService(context.Background(), load, &stderr)

	if code == 0 {
		t.Error("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "TELEGRAM_BOT_TOKEN") {
		t.Errorf("stderr %q should name TELEGRAM_BOT_TOKEN", stderr.String())
	}
}

func TestRunDispatchesRun(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run"}, &stdout, &stderr); code == 0 {
		t.Error("exit code = 0, want non-zero without configuration")
	}
}

func TestNewLogger(t *testing.T) {
	log, err := newLogger("warn")
	if err != nil {
		t.Fatal(err)
	}
	if log.Core().Enabled(zapcore.InfoLevel) {
		t.Error("info enabled at warn level")
	}
	if !log.Core().Enabled(zapcore.ErrorLevel) {
		t.Error("error disabled at warn level")
	}
	if _, err := newLogger("loud"); err == nil {
		t.Error("want error for an unknown level")
	}
}
