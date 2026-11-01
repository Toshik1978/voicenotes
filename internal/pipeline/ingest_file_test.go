package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

type fakeAppender struct {
	nextID  int64
	got     capture.Capture
	quiet   time.Duration
	created bool
	moved   bool
	err     error
	// joinOnly counts JoinOpenBatch calls; noOpen makes them find no batch.
	joinOnly int
	noOpen   bool
}

func (f *fakeAppender) JoinOpenBatch(
	ctx context.Context,
	item capture.Capture,
	quiet time.Duration,
) (capture.AppendResult, bool, error) {
	f.joinOnly++
	if f.noOpen {
		f.got, f.quiet = item, quiet

		return capture.AppendResult{}, false, f.err
	}
	res, err := f.AppendFile(ctx, item, quiet)

	return res, err == nil, err
}

func (f *fakeAppender) AppendFile(
	_ context.Context,
	item capture.Capture,
	quiet time.Duration,
) (capture.AppendResult, error) {
	f.got, f.quiet = item, quiet
	if f.err != nil {
		return capture.AppendResult{}, f.err
	}
	f.nextID++
	item.ID = f.nextID

	return capture.AppendResult{Capture: item, Created: f.created, DeadlineMoved: f.moved}, nil
}

func fileItem() pipeline.FileItem {
	return pipeline.FileItem{
		Source: "telegram", ChatID: -1001234567890, MessageID: 7,
		ReceivedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		Kind:       capture.FileDocument, FileID: "FAKE-FILE", MimeType: "application/pdf",
		Name: "a.pdf", Size: 10, Caption: "cap", AlbumID: "alb",
	}
}

func newFileIngester(t *testing.T, app *fakeAppender, w *fakeWaker) *pipeline.Ingester {
	t.Helper()

	in, err := pipeline.NewIngester(&fakeInserter{}, w, pipeline.WithFiles(app, 3*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	return in
}

func TestIngestFileBuildsCollectCapture(t *testing.T) {
	app, w := &fakeAppender{created: true, moved: true}, &fakeWaker{}
	in := newFileIngester(t, app, w)
	item := fileItem()
	item.LocalPath = "files/a.pdf"

	var acked []capture.Capture
	got, err := in.IngestFile(context.Background(), item, func(_ context.Context, c capture.Capture) {
		acked = append(acked, c)
	})
	if err != nil {
		t.Fatal(err)
	}
	c := app.got
	if c.Kind != capture.KindFiles || c.Step != capture.StepCollect || c.Text != "cap" || c.AlbumID != "alb" {
		t.Fatalf("capture = %+v", c)
	}
	if c.SourceChatID != item.ChatID || c.SourceMessageID != item.MessageID || c.Source != "telegram" {
		t.Fatalf("identity = %+v", c)
	}
	// The store owns the deadline (capture.JoinBatch); ingest only passes quiet.
	if app.quiet != 3*time.Second || !c.ReceivedAt.Equal(item.ReceivedAt) {
		t.Fatalf("quiet = %v received = %v", app.quiet, c.ReceivedAt)
	}
	if len(c.Files) != 1 {
		t.Fatalf("files = %+v", c.Files)
	}
	f := c.Files[0]
	if f.Position != 0 || f.State != capture.FilePending || f.Kind != capture.FileDocument ||
		f.FileID != "FAKE-FILE" || f.MimeType != "application/pdf" || f.FileName != "a.pdf" || f.FileSize != 10 ||
		f.LocalPath != "files/a.pdf" || f.SourceMessageID != 7 {
		t.Fatalf("file = %+v", f)
	}
	if got.ID != 1 || len(acked) != 1 || acked[0].ID != 1 || w.n != 1 {
		t.Fatalf("got=%+v acked=%d wakes=%d", got, len(acked), w.n)
	}
}

func TestIngestFileJoinSkipsAckAndWakesOnlyWhenDeadlineMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		moved bool
		wakes int
	}{
		"deadline moved": {moved: true, wakes: 1},
		// The hold was not deferred, so its release does not wake either.
		"deadline kept": {moved: false, wakes: 0},
	} {
		t.Run(name, func(t *testing.T) {
			app, w := &fakeAppender{created: false, moved: tc.moved}, &fakeWaker{}
			in := newFileIngester(t, app, w)

			acked := false
			_, err := in.IngestFile(
				context.Background(),
				fileItem(),
				func(context.Context, capture.Capture) { acked = true },
			)
			if err != nil {
				t.Fatal(err)
			}
			if acked || w.n != tc.wakes {
				t.Fatalf("acked=%v wakes=%d, want no ack and %d wakes", acked, w.n, tc.wakes)
			}
		})
	}
}

func TestIngestFileDuplicateAndError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want func(error) bool
	}{
		"duplicate": {dupError{}, func(e error) bool { return errors.Is(e, pipeline.ErrDuplicate) }},
		"other":     {errStore, func(e error) bool { return errors.Is(e, errStore) }},
	} {
		t.Run(name, func(t *testing.T) {
			w := &fakeWaker{}
			in := newFileIngester(t, &fakeAppender{err: tc.err}, w)
			acked := false
			_, err := in.IngestFile(
				context.Background(),
				fileItem(),
				func(context.Context, capture.Capture) { acked = true },
			)
			if !tc.want(err) || acked || w.n != 0 {
				t.Fatalf("err=%v acked=%v wakes=%d", err, acked, w.n)
			}
		})
	}
}

func TestIngestFileHoldsAckBeforeStoreAndWakesAfter(t *testing.T) {
	var log []string
	w := &fakeWaker{log: &log}
	in := newFileIngester(t, &fakeAppender{created: true}, w)

	_, err := in.IngestFile(
		context.Background(),
		fileItem(),
		func(context.Context, capture.Capture) { log = append(log, "ack") },
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hold telegram/-1001234567890/7", "ack", "release", "wake"}
	if len(log) != len(want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("log = %v, want %v", log, want)
		}
	}
}

func TestIngestFileRequiresConfiguration(t *testing.T) {
	in, err := pipeline.NewIngester(&fakeInserter{}, &fakeWaker{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.IngestFile(context.Background(), fileItem(), nil); err == nil {
		t.Fatal("want error without WithFiles")
	}
}

func TestIngestFileTooLargeAlbumItemIsAppendedFailed(t *testing.T) {
	app, w := &fakeAppender{created: true, moved: true}, &fakeWaker{}
	in := newFileIngester(t, app, w)
	item := fileItem()
	item.TooLarge = true

	acked := false
	ack := func(context.Context, capture.Capture) { acked = true }
	if _, err := in.IngestFile(context.Background(), item, ack); err != nil {
		t.Fatal(err)
	}
	f := app.got.Files[0]
	if app.joinOnly != 0 || f.State != capture.FileFailed || f.Failure != "too large for Telegram bots" {
		t.Fatalf("joinOnly=%d file=%+v, want an appended failed file", app.joinOnly, f)
	}
	// The album's first item created the batch: it carries the ⏳.
	if !acked || w.n != 1 {
		t.Fatalf("acked=%v wakes=%d", acked, w.n)
	}
}

func TestIngestFileTooLargeStandaloneItemOnlyJoinsAnOpenBatch(t *testing.T) {
	t.Run("open batch", func(t *testing.T) {
		app, w := &fakeAppender{moved: true}, &fakeWaker{}
		in := newFileIngester(t, app, w)
		item := fileItem()
		item.AlbumID, item.TooLarge = "", true

		if _, err := in.IngestFile(context.Background(), item, nil); err != nil {
			t.Fatal(err)
		}
		if app.joinOnly != 1 || app.got.Files[0].State != capture.FileFailed || w.n != 1 {
			t.Fatalf("joinOnly=%d file=%+v wakes=%d", app.joinOnly, app.got.Files[0], w.n)
		}
	})
	t.Run("no open batch", func(t *testing.T) {
		app, w := &fakeAppender{noOpen: true}, &fakeWaker{}
		in := newFileIngester(t, app, w)
		item := fileItem()
		item.AlbumID, item.TooLarge = "", true

		acked := false
		_, err := in.IngestFile(context.Background(), item, func(context.Context, capture.Capture) { acked = true })
		if !errors.Is(err, pipeline.ErrFileTooLarge) || acked || w.n != 0 {
			t.Fatalf("err=%v acked=%v wakes=%d, want ErrFileTooLarge, no ack, no wake", err, acked, w.n)
		}
	})
}
