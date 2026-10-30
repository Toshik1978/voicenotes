package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
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

func TestDigestGoroutinesOnlyWhenEnabled(t *testing.T) {
	cfg := config.Config{
		DigestChatID: 1, DigestTime: "07:30", Location: time.UTC,
		DigestCatchup: time.Hour, DigestMaxNotes: 5,
	}
	got, err := digestGoroutines(cfg, zap.NewNop(), nil, nil, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("disabled: %d goroutines, %v", len(got), err)
	}
	cfg.DigestEnabled = true
	if got, err = digestGoroutines(cfg, zap.NewNop(), nil, nil, nil); err != nil || len(got) != 1 {
		t.Fatalf("enabled: %d goroutines, %v", len(got), err)
	}
	cfg.DigestTime = "bad"
	if _, err = digestGoroutines(cfg, zap.NewNop(), nil, nil, nil); err == nil {
		t.Fatal("bad config: want error")
	}
}

func TestBuildSweepsAgedOrphanAudio(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audio, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(audio, "tg-1-2-3.ogg")
	if err := os.WriteFile(orphan, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Parse(map[string]string{
		"TELEGRAM_BOT_TOKEN":     "123456:FAKE-TOKEN",
		"TELEGRAM_ALLOWED_USERS": "111",
		"STT_BASE_URL":           "http://stt.example.org:8000/v1",
		"STT_MODEL":              "whisper-1",
		"MEMOS_URL":              "http://memos.example.org:5230",
		"MEMOS_TOKEN":            "memos_pat_FAKE",
		"DATA_DIR":               dir,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	svc, err := build(cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = svc.closeStore() })

	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aged orphan survived build: %v", err)
	}
}
