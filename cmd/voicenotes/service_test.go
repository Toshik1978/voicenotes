package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/config"
	"github.com/Toshik1978/voicenotes/internal/health"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/telegram"
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
	svc, err := build(cfg, zap.NewNop(), time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = svc.closeStore() })

	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aged orphan survived build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audio")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy audio dir survived the migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "files")); err != nil {
		t.Errorf("files dir missing: %v", err)
	}
}

type fakeWaker struct {
	wakes, holds, releases int
}

func (f *fakeWaker) Wake() { f.wakes++ }
func (f *fakeWaker) HoldAck(capture.Capture) func() {
	f.holds++
	return func() { f.releases++ }
}

func TestWakeAllReachesEveryLane(t *testing.T) {
	a, b := &fakeWaker{}, &fakeWaker{}
	w := wakeAll{a, b}

	w.Wake()
	release := w.HoldAck(capture.Capture{})
	release()

	for i, f := range []*fakeWaker{a, b} {
		if f.wakes != 1 || f.holds != 1 || f.releases != 1 {
			t.Errorf("lane %d: wakes=%d holds=%d releases=%d, want 1 each", i, f.wakes, f.holds, f.releases)
		}
	}
}

// voiceTranscriber returns the same transcript for every voice capture.
type voiceTranscriber struct{ text string }

func (v voiceTranscriber) Transcribe(
	context.Context, pipeline.TranscribeRequest,
) (pipeline.TranscribeResult, capture.Outcome) {
	return pipeline.TranscribeResult{Text: v.text}, capture.OK()
}

// fakeDocling is an httptest docling-serve. Each submit is announced on
// submitted; with a release channel, the submit answers only once it is
// closed, so a test controls exactly when the conversion ends.
type fakeDocling struct {
	srv       *httptest.Server
	submitted chan struct{}
	release   chan struct{}
	unblock   func()
}

func newFakeDocling(t *testing.T, markdown string, blocking bool) *fakeDocling {
	t.Helper()
	d := &fakeDocling{submitted: make(chan struct{}, 16), unblock: func() {}}
	if blocking {
		d.release = make(chan struct{})
		var once sync.Once
		d.unblock = func() { once.Do(func() { close(d.release) }) }
	}
	result, err := json.Marshal(map[string]any{
		"kind": "ConvertDocumentResponse", "status": "success",
		"document": map[string]any{"md_content": markdown},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/convert/file/async", func(w http.ResponseWriter, r *http.Request) {
		d.submitted <- struct{}{}
		if d.release != nil {
			select {
			case <-d.release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, `{"task_id":"task-1"}`)
	})
	mux.HandleFunc("GET /v1/status/poll/task-1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"task_status":"success"}`)
	})
	mux.HandleFunc("GET /v1/result/task-1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(result)
	})
	d.srv = httptest.NewServer(mux)
	// Cleanups run last-in first-out: release a blocked submit before Close
	// waits for it.
	t.Cleanup(d.srv.Close)
	t.Cleanup(d.unblock)

	return d
}

// documentMessage is a PDF sent as a document.
func documentMessage(id int64) telegram.Message {
	return telegram.Message{
		ChatID: 7, ID: id, SenderID: 111, Date: time.Now(),
		Files: &telegram.FileItem{
			FileID: "doc-1", MIME: "application/pdf", Name: "report.pdf", Kind: capture.FileDocument, Size: 10,
		},
	}
}

func voiceMessage(id int64) telegram.Message {
	return telegram.Message{
		ChatID: 7, ID: id, SenderID: 111, Date: time.Now(),
		Audio: &telegram.Audio{FileID: "voice-1", MimeType: "audio/ogg", Size: 10, Voice: true},
	}
}

// FR-114a: a document waiting on a slow conversion never delays a later
// voice capture's note.
func TestVoiceNoteCompletesWhileDocumentWaitsOnDocling(t *testing.T) {
	docling := newFakeDocling(t, "Quarterly figures", true)
	cfg := e2eConfig()
	cfg.DoclingURL = docling.srv.URL
	e := startE2E(t, cfg, voiceTranscriber{text: "buy milk"})

	e.handler.Handle(t.Context(), documentMessage(41))
	select {
	case <-docling.submitted:
	case <-time.After(10 * time.Second):
		t.Fatal("the document never reached docling")
	}

	e.handler.Handle(t.Context(), voiceMessage(42))
	waitFor(t, "the voice note", func() bool {
		notes, _, _ := e.world.snapshot()
		return len(notes) > 0
	})
	notes, _, _ := e.world.snapshot()
	if len(notes) != 1 || !strings.Contains(notes[0].Content, "buy milk") {
		t.Fatalf("notes while docling is blocked = %+v, want only the voice note", notes)
	}

	docling.unblock()
	waitFor(t, "the document note", func() bool {
		got, _, _ := e.world.snapshot()
		return len(got) == 2
	})
	notes, _, _ = e.world.snapshot()
	if !strings.Contains(notes[1].Content, "Quarterly figures") {
		t.Errorf("second note = %q, want the document note", notes[1].Content)
	}
}

// FR-128: an unreachable docling leaves the document waiting on docling, and
// health degrades once that wait passes the threshold.
func TestUnreachableDoclingDegradesHealthPastThreshold(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	cfg := e2eConfig()
	cfg.DoclingURL = down.URL
	e := startE2E(t, cfg, e2eTranscriber{})

	e.handler.Handle(t.Context(), documentMessage(41))
	queue := healthQueue{db: e.db}
	waitFor(t, "the capture to wait on docling", func() bool {
		o, err := queue.OldestUnfinished(t.Context())
		return err == nil && o != nil && o.WaitingOn == string(capture.WaitingDocling)
	})

	const threshold = time.Hour
	snapshot := func(now time.Time) health.Report {
		clock := func() time.Time { return now }
		return health.Compute(health.NewCollector(health.CollectorConfig{
			Queue: queue, Worker: bothLanes{e.p.mainWorker, e.p.docsWorker}, LastPoll: clock,
			Clock: clock, Started: time.Now(), Threshold: threshold,
		}).Snapshot(t.Context()))
	}
	if rep := snapshot(time.Now()); rep.Status != health.StatusOK || rep.WaitingOn != "docling" {
		t.Errorf("within the threshold: %+v, want ok waiting on docling", rep)
	}
	if rep := snapshot(time.Now().Add(2 * threshold)); rep.Status != health.StatusDegraded ||
		rep.WaitingOn != "docling" {
		t.Errorf("past the threshold: %+v, want degraded waiting on docling", rep)
	}
}

// FR-128: without DOCLING_URL a document is never read, so nothing ever waits
// on docling and the capture still ends in a note.
func TestNoDoclingURLNeverWaitsOnDocling(t *testing.T) {
	e := startE2E(t, e2eConfig(), e2eTranscriber{})
	queue := healthQueue{db: e.db}

	e.handler.Handle(t.Context(), documentMessage(41))
	waitFor(t, "the document note", func() bool {
		if o, err := queue.OldestUnfinished(t.Context()); err == nil && o != nil &&
			o.WaitingOn == string(capture.WaitingDocling) {
			t.Fatalf("a capture waits on docling without DOCLING_URL: %+v", o)
		}
		notes, _, _ := e.world.snapshot()

		return len(notes) == 1
	})
}

// HINT_EXCERPT_RUNES reaches the hint request: the document text the LLM gets
// is cut to exactly that many runes.
func TestHintExcerptRunesBoundsTheLLMRequest(t *testing.T) {
	docling := newFakeDocling(t, strings.Repeat("ж", 5000), false)
	var (
		mu       sync.Mutex
		requests []string
	)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			for _, m := range req.Messages {
				if m.Role == "user" {
					mu.Lock()
					requests = append(requests, m.Content)
					mu.Unlock()
				}
			}
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"hint\":\"A long report\"}"}}]}`)
	}))
	t.Cleanup(llm.Close)

	cfg := e2eConfig()
	cfg.DoclingURL = docling.srv.URL
	cfg.LLMEnabled, cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMTimeout = true, llm.URL, "fake-model", 5*time.Second
	cfg.TargetLanguage, cfg.HintExcerptRunes = "en", 500
	e := startE2E(t, cfg, e2eTranscriber{})

	e.handler.Handle(t.Context(), documentMessage(41))
	waitFor(t, "the document note", func() bool {
		notes, _, _ := e.world.snapshot()
		return len(notes) == 1
	})

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("LLM got %d hint requests, want 1", len(requests))
	}
	if n := strings.Count(requests[0], "ж"); n != 500 {
		t.Errorf("excerpt runes in the request = %d, want 500 (HINT_EXCERPT_RUNES)", n)
	}
}
