package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// log records RestartQuiet calls; restartErr fails them.
	log        *[]string
	restarts   int
	restartErr error
	// localErr fails SetFileLocalPath calls.
	localErr error
}

func (f *fakeAppender) SetFileLocalPath(_ context.Context, id int64, position int, name string) error {
	if f.log != nil {
		*f.log = append(*f.log, fmt.Sprintf("local %d %d %s", id, position, name))
	}

	return f.localErr
}

func (f *fakeAppender) RestartQuiet(_ context.Context, id int64, quiet time.Duration) error {
	f.restarts++
	if f.log != nil {
		*f.log = append(*f.log, fmt.Sprintf("restart %d %s", id, quiet))
	}

	return f.restartErr
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

// arrivalFetch is the Downloader IngestFile uses for a batch's first item.
type arrivalFetch struct {
	log      *[]string
	err      error
	calls    int
	deadline time.Duration // time left on the ctx at the call; 0 for none
}

func (f *arrivalFetch) Download(ctx context.Context, fileID string) ([]byte, error) {
	f.calls++
	if d, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(d)
	}
	if f.log != nil {
		*f.log = append(*f.log, "download "+fileID)
	}

	return []byte("bytes of " + fileID), f.err
}

// loggedFiles is DirFiles that logs writes and removals and can fail writes.
type loggedFiles struct {
	*pipeline.DirFiles

	log      *[]string
	writeErr error
}

func (f *loggedFiles) Write(name string, data []byte) error {
	if f.log != nil {
		*f.log = append(*f.log, "write "+name)
	}
	if f.writeErr != nil {
		return f.writeErr
	}

	return f.DirFiles.Write(name, data)
}

func (f *loggedFiles) Remove(name string) error {
	if f.log != nil {
		*f.log = append(*f.log, "remove "+name)
	}

	return f.DirFiles.Remove(name)
}

func newFileIngester(t *testing.T, app *fakeAppender, w *fakeWaker) *pipeline.Ingester {
	t.Helper()

	return newFetchingIngester(t, app, w, &arrivalFetch{}, &loggedFiles{DirFiles: pipeline.NewDirFiles(t.TempDir())})
}

func newFetchingIngester(
	t *testing.T, app *fakeAppender, w *fakeWaker, fetch pipeline.Downloader, files pipeline.FileStore,
) *pipeline.Ingester {
	t.Helper()

	in, err := pipeline.NewIngester(&fakeInserter{}, w, pipeline.WithFiles(app, 3*time.Second, fetch, files))
	if err != nil {
		t.Fatal(err)
	}

	return in
}

func wantLog(t *testing.T, log, want []string) {
	t.Helper()

	if len(log) != len(want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("log = %v, want %v", log, want)
		}
	}
}

// Constitution VI, FR-103: the item that creates a batch is downloaded,
// written durably and recorded before its ⏳, so the acknowledged capture
// is held locally.
func TestIngestFileKeepsTheCreatingItemLocallyBeforeAck(t *testing.T) {
	var log []string
	dir := t.TempDir()
	app, w := &fakeAppender{created: true, moved: true, log: &log}, &fakeWaker{log: &log}
	fetch := &arrivalFetch{log: &log}
	in := newFetchingIngester(t, app, w, fetch, &loggedFiles{DirFiles: pipeline.NewDirFiles(dir), log: &log})

	var onDisk string
	var ackedPath string
	_, err := in.IngestFile(context.Background(), fileItem(), func(_ context.Context, c capture.Capture) {
		data, rerr := os.ReadFile(filepath.Join(dir, "1.pdf"))
		if rerr != nil {
			t.Errorf("at ⏳ the file is not on disk: %v", rerr)
		}
		onDisk, ackedPath = string(data), c.Files[0].LocalPath
		log = append(log, "ack")
	})
	if err != nil {
		t.Fatal(err)
	}
	wantLog(t, log, []string{
		"hold telegram/-1001234567890/7", "download FAKE-FILE", "write 1.pdf", "local 1 0 1.pdf",
		"ack", "restart 1 3s", "release", "wake",
	})
	if onDisk != "bytes of FAKE-FILE" || ackedPath != "1.pdf" {
		t.Fatalf("at ⏳: data %q, local path %q", onDisk, ackedPath)
	}
	// The arrival download is bounded, so a slow one leaves the file to the
	// download step instead of stalling the chat.
	if fetch.deadline <= 0 || fetch.deadline > pipeline.ArrivalDownloadLimit {
		t.Fatalf("download ctx deadline in %v, want within %v", fetch.deadline, pipeline.ArrivalDownloadLimit)
	}
}

// A first item that cannot be held locally stays stored without data and
// gets no ⏳ here: the download step fetches it and acknowledges then. The
// quiet period still restarts, since the attempt may have outlasted it.
func TestIngestFileArrivalDownloadFailureSkipsTheAck(t *testing.T) {
	for name, tc := range map[string]struct {
		fetchErr, writeErr, localErr error
		want                         []string
	}{
		"download": {fetchErr: errStore, want: []string{"download FAKE-FILE", "restart 1 3s"}},
		"write":    {writeErr: errStore, want: []string{"download FAKE-FILE", "write 1.pdf", "restart 1 3s"}},
		"record": {localErr: errStore, want: []string{
			"download FAKE-FILE", "write 1.pdf", "local 1 0 1.pdf", "remove 1.pdf", "restart 1 3s",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var log []string
			dir := t.TempDir()
			app := &fakeAppender{created: true, moved: true, log: &log, localErr: tc.localErr}
			w := &fakeWaker{}
			files := &loggedFiles{DirFiles: pipeline.NewDirFiles(dir), log: &log, writeErr: tc.writeErr}
			in := newFetchingIngester(t, app, w, &arrivalFetch{log: &log, err: tc.fetchErr}, files)

			acked := false
			stored, err := in.IngestFile(context.Background(), fileItem(),
				func(context.Context, capture.Capture) { acked = true })
			if err != nil || stored.ID != 1 || acked || w.n != 1 {
				t.Fatalf("stored=%d err=%v acked=%v wakes=%d, want stored, no ack, woken", stored.ID, err, acked, w.n)
			}
			wantLog(t, log, tc.want)
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("files left behind: %v", entries)
			}
		})
	}
}

// Only the creating item is fetched on arrival: a joining item stays
// row-first, an item already held is not fetched again, and a too-large
// one has nothing to fetch.
func TestIngestFileFetchesOnlyTheCreatingItem(t *testing.T) {
	for name, tc := range map[string]struct {
		created bool
		item    func(pipeline.FileItem) pipeline.FileItem
		acked   bool
	}{
		"joining":   {created: false, item: func(i pipeline.FileItem) pipeline.FileItem { return i }},
		"held":      {created: true, acked: true, item: func(i pipeline.FileItem) pipeline.FileItem { i.LocalPath = "x.pdf"; return i }},
		"too large": {created: true, acked: true, item: func(i pipeline.FileItem) pipeline.FileItem { i.TooLarge = true; return i }},
	} {
		t.Run(name, func(t *testing.T) {
			fetch := &arrivalFetch{}
			app := &fakeAppender{created: tc.created, moved: true}
			in := newFetchingIngester(t, app, &fakeWaker{}, fetch,
				&loggedFiles{DirFiles: pipeline.NewDirFiles(t.TempDir())})

			acked := false
			ack := func(context.Context, capture.Capture) { acked = true }
			if _, err := in.IngestFile(context.Background(), tc.item(fileItem()), ack); err != nil {
				t.Fatal(err)
			}
			if fetch.calls != 0 || acked != tc.acked {
				t.Fatalf("downloads=%d acked=%v, want none and acked=%v", fetch.calls, acked, tc.acked)
			}
		})
	}
}

func TestWithFilesRequiresTheArrivalFetch(t *testing.T) {
	files := pipeline.NewDirFiles(t.TempDir())
	for name, opt := range map[string]pipeline.IngesterOption{
		"downloader": pipeline.WithFiles(&fakeAppender{}, time.Second, nil, files),
		"file store": pipeline.WithFiles(&fakeAppender{}, time.Second, &arrivalFetch{}, nil),
	} {
		if _, err := pipeline.NewIngester(&fakeInserter{}, &fakeWaker{}, opt); err == nil {
			t.Errorf("nil %s: want error", name)
		}
	}
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
			if acked || w.n != tc.wakes || app.restarts != 0 {
				t.Fatalf("acked=%v wakes=%d restarts=%d, want no ack, %d wakes and no restart",
					acked, w.n, app.restarts, tc.wakes)
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
	// vn-xht.8: the store reports a redelivered item as a duplicate.
	t.Run("redelivered", func(t *testing.T) {
		app, w := &fakeAppender{noOpen: true, err: dupError{}}, &fakeWaker{}
		in := newFileIngester(t, app, w)
		item := fileItem()
		item.AlbumID, item.TooLarge = "", true

		_, err := in.IngestFile(context.Background(), item, nil)
		if !errors.Is(err, pipeline.ErrDuplicate) || w.n != 0 {
			t.Fatalf("err=%v wakes=%d, want ErrDuplicate and no wake", err, w.n)
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

// A failed quiet-period restart cannot fail the ingest: the row is stored
// and acknowledged, and the worst case is the deadline set at the append.
func TestIngestFileRestartFailureKeepsTheIngest(t *testing.T) {
	app, w := &fakeAppender{created: true, restartErr: errStore}, &fakeWaker{}
	in := newFileIngester(t, app, w)

	acked := false
	stored, err := in.IngestFile(context.Background(), fileItem(),
		func(context.Context, capture.Capture) { acked = true })
	if err != nil || stored.ID != 1 || !acked || app.restarts != 1 || w.n != 1 {
		t.Fatalf("stored=%d err=%v acked=%v restarts=%d wakes=%d", stored.ID, err, acked, app.restarts, w.n)
	}
}

// Without an ack nothing delayed the batch, so its deadline stays as set.
func TestIngestFileWithoutAckDoesNotRestartTheQuietPeriod(t *testing.T) {
	app := &fakeAppender{created: true}
	in := newFileIngester(t, app, &fakeWaker{})

	if _, err := in.IngestFile(context.Background(), fileItem(), nil); err != nil || app.restarts != 0 {
		t.Fatalf("err=%v restarts=%d", err, app.restarts)
	}
}
