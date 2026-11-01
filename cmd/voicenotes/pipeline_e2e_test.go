package main

import (
	"context"
	"fmt"
	"slices"
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
	// attached holds each attachment's bytes, which name its file ID.
	attached []string
	replies  []string
}

func (w *e2eWorld) Create(_ context.Context, n pipeline.NoteInput) (string, string, capture.Outcome) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notes = append(w.notes, n)

	return "memos/1", "https://memos.example.org/m/1", capture.OK()
}

func (w *e2eWorld) Attach(_ context.Context, _, filename, _ string, data []byte) capture.Outcome {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attachments = append(w.attachments, filename)
	w.attached = append(w.attached, string(data))

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

func (w *e2eWorld) Download(_ context.Context, fileID string) ([]byte, error) {
	return []byte("jpeg " + fileID), nil
}

func (w *e2eWorld) attachedData() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]string(nil), w.attached...)
}

func (w *e2eWorld) snapshot() (notes []pipeline.NoteInput, attachments, replies []string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append(notes, w.notes...), append(attachments, w.attachments...), append(replies, w.replies...)
}

// e2e is a running pipeline behind the Telegram handler, wired by
// newPipeline against the fake world.
type e2e struct {
	world   *e2eWorld
	db      *sqlite.DB
	p       *pipelineParts
	handler *telegram.Handler
}

// e2eConfig is the minimal configuration the pipeline needs; tests add the
// optional features they exercise.
func e2eConfig() config.Config {
	return config.Config{
		InboxTag: "inbox", TodoTag: "todo", MemosContentLimit: 10000,
		BatchQuietPeriod: 50 * time.Millisecond, RetryMaxInterval: time.Second, Location: time.UTC,
		DoclingTimeout: time.Minute, DoclingMaxPages: 20, HintExcerptRunes: 4000,
	}
}

// startE2E wires the pipeline with cfg and runs both lane workers until the
// test ends.
func startE2E(t *testing.T, cfg config.Config, transcriber pipeline.Transcriber) *e2e {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	world := &e2eWorld{}
	filesDir := dir + "/files"
	deps := pipeline.Deps{
		Transcriber: transcriber, Notes: world, Replier: world, Downloader: world,
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

	// t.Context is cancelled before the cleanups run, so the workers stop
	// before the store closes.
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	for _, w := range []*pipeline.Worker{p.mainWorker, p.docsWorker} {
		wg.Go(func() { w.Run(t.Context()) })
	}

	return &e2e{world: world, db: db, p: p, handler: handler}
}

// waitFor polls cond until it holds, failing the test after 10s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPhotoFlowsFromHandlerToSavedNote(t *testing.T) {
	e := startE2E(t, e2eConfig(), e2eTranscriber{})
	world := e.world

	e.handler.Handle(t.Context(), telegram.Message{
		ChatID: 7, ID: 42, SenderID: 111, Date: time.Now(),
		Files: &telegram.FileItem{FileID: "photo-1", MIME: "image/jpeg", Kind: capture.FileImage, Size: 10},
	})

	waitFor(t, "the saved reply", func() bool {
		_, _, replies := world.snapshot()
		return len(replies) > 0 && strings.HasPrefix(replies[len(replies)-1], "✓ saved")
	})

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

// photo is one compressed photo of album (empty for a standalone one).
func photo(id int64, album, caption string) telegram.Message {
	return telegram.Message{
		ChatID: 7, ID: id, SenderID: 111, Date: time.Now(),
		Files: &telegram.FileItem{
			FileID: fmt.Sprintf("photo-%d", id), MIME: "image/jpeg", Kind: capture.FileImage, Size: 10,
			Caption: caption, AlbumID: album,
		},
	}
}

// batchConfig leaves room for a slow -race run to deliver a whole send
// within one quiet period.
func batchConfig() config.Config {
	cfg := e2eConfig()
	cfg.BatchQuietPeriod = 500 * time.Millisecond

	return cfg
}

// waitForIdle waits until every capture is done and returns the world.
func (e *e2e) waitForIdle(t *testing.T) (notes []pipeline.NoteInput, attachments, replies []string) {
	t.Helper()
	waitFor(t, "an empty queue", func() bool {
		n, err := e.db.CountUnfinished(t.Context())
		return err == nil && n == 0
	})

	return e.world.snapshot()
}

func count(replies []string, prefix string) int {
	n := 0
	for _, r := range replies {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}

	return n
}

// US3 scenario 1-2: one album, one ⏳, one note with every photo in order
// and the 3rd item's caption as its text.
func TestAlbumWithCaptionOnThirdItemYieldsOneNote(t *testing.T) {
	e := startE2E(t, batchConfig(), e2eTranscriber{})
	for id := int64(1); id <= 4; id++ {
		caption := ""
		if id == 3 {
			caption = "Receipts from the trip"
		}
		e.handler.Handle(t.Context(), photo(id, "FAKE-ALBUM-1", caption))
	}

	notes, _, replies := e.waitForIdle(t)
	if len(notes) != 1 || !strings.HasPrefix(notes[0].Content, "Receipts from the trip") ||
		!strings.Contains(notes[0].Content, "📷 4 photos") {
		t.Fatalf("notes = %+v, want one note with the caption and one hint", notes)
	}
	want := []string{"jpeg photo-1", "jpeg photo-2", "jpeg photo-3", "jpeg photo-4"}
	if got := e.world.attachedData(); !slices.Equal(got, want) {
		t.Errorf("attachments = %q, want %q", got, want)
	}
	if count(replies, "⏳") != 1 || count(replies, "✓ saved") != 1 {
		t.Errorf("replies = %q, want one ⏳ edited into one saved reply", replies)
	}
}

// US3 scenario 7: 15 photos arrive as two albums and still make one note.
func TestFifteenPhotosAcrossTwoAlbumsYieldOneNote(t *testing.T) {
	e := startE2E(t, batchConfig(), e2eTranscriber{})
	for id := int64(1); id <= 15; id++ {
		album := "FAKE-ALBUM-1"
		if id > 10 {
			album = "FAKE-ALBUM-2"
		}
		e.handler.Handle(t.Context(), photo(id, album, ""))
	}

	notes, attachments, replies := e.waitForIdle(t)
	if len(notes) != 1 || len(attachments) != 15 {
		t.Fatalf("%d notes, %d attachments; want 1 note with 15", len(notes), len(attachments))
	}
	if count(replies, "⏳") != 1 || count(replies, "✓ saved") != 1 {
		t.Errorf("replies = %q, want one ⏳ and one saved reply", replies)
	}
}

// US3 scenario 9: a voice message within the quiet period never joins the
// photo's batch.
func TestPhotoThenVoiceYieldTwoNotes(t *testing.T) {
	e := startE2E(t, batchConfig(), voiceTranscriber{text: "buy milk"})
	e.handler.Handle(t.Context(), photo(1, "", ""))
	e.handler.Handle(t.Context(), voiceMessage(2))

	notes, attachments, replies := e.waitForIdle(t)
	if len(notes) != 2 || len(attachments) != 2 {
		t.Fatalf("%d notes, %d attachments; want 2 notes with one each", len(notes), len(attachments))
	}
	if count(replies, "⏳") != 2 || count(replies, "✓ saved") != 2 {
		t.Errorf("replies = %q, want a ⏳ and a saved reply per note", replies)
	}
}

// US3 scenario 6: an album item over the download limit is kept as failed;
// the note keeps the others and the reply names it.
func TestAlbumKeepsOtherItemsWhenOneIsTooLarge(t *testing.T) {
	e := startE2E(t, batchConfig(), e2eTranscriber{})
	for id := int64(1); id <= 4; id++ {
		m := photo(id, "FAKE-ALBUM-1", "")
		if id == 2 {
			m.Files.Size = telegram.MaxFileSize + 1
		}
		e.handler.Handle(t.Context(), m)
	}

	notes, attachments, replies := e.waitForIdle(t)
	if len(notes) != 1 || len(attachments) != 3 {
		t.Fatalf("%d notes, %d attachments; want 1 note with 3", len(notes), len(attachments))
	}
	if count(replies, "⏳") != 1 || count(replies, "⚠ saved, 1 of 4 not kept — too large for Telegram bots") != 1 {
		t.Errorf("replies = %q, want one ⏳ and a reply naming the item not kept", replies)
	}
}
