package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/config"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
	"github.com/Toshik1978/voicenotes/internal/telegram"
)

type e2eTranscriber struct{}

func (e2eTranscriber) Transcribe(
	context.Context,
	pipeline.TranscribeRequest,
) (pipeline.TranscribeResult, capture.Outcome) {
	return pipeline.TranscribeResult{}, capture.Permanent("unused")
}

// e2eWorld is the fake outside world: the notes store, Telegram replies and
// file downloads.
type e2eWorld struct {
	mu          sync.Mutex
	notes       []pipeline.NoteInput
	attachments []string
	replies     []string
}

func (w *e2eWorld) Create(_ context.Context, n pipeline.NoteInput) (string, string, capture.Outcome) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notes = append(w.notes, n)

	return "memos/1", "https://memos.example.org/m/1", capture.OK()
}

func (w *e2eWorld) Attach(_ context.Context, _, filename, _ string, _ []byte) capture.Outcome {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attachments = append(w.attachments, filename)

	return capture.OK()
}

func (w *e2eWorld) Send(_ context.Context, _, _ int64, text string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.replies = append(w.replies, text)

	return 500, nil
}

func (w *e2eWorld) Edit(_ context.Context, _, msgID int64, text string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.replies = append(w.replies, text)

	return msgID, nil
}

func (w *e2eWorld) Download(context.Context, string) ([]byte, error) {
	return []byte("jpeg bytes"), nil
}

func (w *e2eWorld) snapshot() (notes []pipeline.NoteInput, attachments, replies []string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append(notes, w.notes...), append(attachments, w.attachments...), append(replies, w.replies...)
}

func TestPhotoFlowsFromHandlerToSavedNote(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	world := &e2eWorld{}
	filesDir := dir + "/files"
	cfg := config.Config{
		InboxTag: "inbox", TodoTag: "todo", MemosContentLimit: 10000,
		BatchQuietPeriod: 50 * time.Millisecond, RetryMaxInterval: time.Second, Location: time.UTC,
	}
	deps := pipeline.Deps{
		Transcriber: e2eTranscriber{}, Notes: world, Replier: world, Downloader: world,
		Files: pipeline.NewDirFiles(filesDir), Log: zap.NewNop(),
	}
	p, err := newPipeline(cfg, zap.NewNop(), db, deps)
	if err != nil {
		t.Fatalf("newPipeline: %v", err)
	}
	handler, err := telegram.NewHandler(telegram.HandlerConfig{
		AllowedUsers: []int64{111}, FilesDir: filesDir, Replier: world, Downloader: world,
		Ingester: p.ingester, Replies: db,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	for _, w := range []*pipeline.Worker{p.mainWorker, p.docsWorker} {
		wg.Go(func() { w.Run(ctx) })
	}

	handler.Handle(ctx, telegram.Message{
		ChatID: 7, ID: 42, SenderID: 111, Date: time.Now(),
		Files: &telegram.FileItem{FileID: "photo-1", MIME: "image/jpeg", Kind: capture.FileImage, Size: 10},
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, _, replies := world.snapshot()
		if len(replies) > 0 && strings.HasPrefix(replies[len(replies)-1], "✓ saved") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no saved reply, replies = %q", replies)
		}
		time.Sleep(10 * time.Millisecond)
	}

	notes, attachments, replies := world.snapshot()
	if len(notes) != 1 || !strings.Contains(notes[0].Content, "📷 photo") {
		t.Errorf("notes = %+v, want one note with the 📷 photo hint", notes)
	}
	if len(attachments) != 1 {
		t.Errorf("attachments = %q, want exactly one", attachments)
	}
	if replies[0] != "⏳" {
		t.Errorf("first reply = %q, want the hourglass acknowledgement", replies[0])
	}
}
