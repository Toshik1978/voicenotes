package sqlite_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/digest"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
)

func ts0() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC) }

func open(t *testing.T, dir string) *sqlite.DB {
	t.Helper()

	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func newCapture(t *testing.T, msgID int64) capture.Capture {
	t.Helper()

	c, err := capture.New(capture.KindAudio, capture.StepDownload, ts0())
	if err != nil {
		t.Fatal(err)
	}
	c.Source = "telegram"
	c.SourceChatID = -1001234567890
	c.SourceMessageID = msgID
	c.Files = []capture.File{{
		Source: c.Source, SourceChatID: c.SourceChatID, SourceMessageID: msgID,
		Kind: capture.FileAudio, State: capture.FilePending, FileID: "FAKE-FILE",
	}}

	return c
}

func TestInsertAssignsIDAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newCapture(t, 1)
	c.Files[0].FileSize, c.Files[0].MimeType = 2048, "audio/ogg"
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if c.ID == 0 {
		t.Fatal("ID not assigned")
	}
	d0, err := s.NextDue(ctx, ts0(), capture.LaneMain)
	got, ok := d0.Capture, d0.OK
	if err != nil || !ok {
		t.Fatalf("NextDue ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip mismatch\n got %+v\nwant %+v", got, c)
	}
	if got.ReceivedAt.Location() != time.UTC {
		t.Fatalf("want UTC, got %v", got.ReceivedAt.Location())
	}
}

func TestInsertDuplicate(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	a := newCapture(t, 1)
	if err := s.Insert(ctx, &a); err != nil {
		t.Fatal(err)
	}
	b := newCapture(t, 1)
	if err := s.Insert(ctx, &b); !errors.Is(err, sqlite.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	c := newCapture(t, 2)
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatalf("different message must insert: %v", err)
	}
}

func TestUpdatePersistsEveryField(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)

	c := newCapture(t, 1)
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	c.Files[0].LocalPath = "a/1.ogg"
	c.Text, c.Language = "hello", "en"
	c.EnrichedJSON, c.NoteName, c.NoteURL = `{"a":1}`, "memos/x", "https://memos.example.org/m/x"
	c.TaskCount, c.ReplyMessageID = 2, 77
	c.Step, c.Status, c.WaitingOn = capture.StepEnrich, capture.StatusWaiting, capture.WaitingMemos
	c.Attempts = 3
	c.NextAttemptAt = ts0().Add(time.Minute)
	c.UpdatedAt = ts0().Add(time.Second)
	c.Failure, c.LLMSkipped = "boom", capture.SkippedUnavailable
	c.Misconfig, c.LastError = true, "http 401"
	c.QueueReplyEdit, c.MinimalNote, c.SendNewReply = true, true, true
	c.EnrichStartedAt = ts0().Add(2 * time.Second)
	if err := s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Reopen to prove durability.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := open(t, dir)
	d0, err := s2.NextDue(ctx, ts0().Add(time.Hour), capture.LaneMain)
	got, ok := d0.Capture, d0.OK
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("mismatch\n got %+v\nwant %+v", got, c)
	}
}

func TestUpdateMissing(t *testing.T) {
	s := open(t, t.TempDir())
	c := newCapture(t, 1)
	c.ID = 999
	if err := s.Update(context.Background(), c); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestNextDueIsDueFirst covers FR-114a: a head that is not due yet no
// longer blocks later due captures; the lane's earliest wake time is
// reported only when nothing is due.
func TestNextDueIsDueFirst(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if r, err := s.NextDue(ctx, ts0(), capture.LaneMain); err != nil || r.OK || !r.WakeAt.IsZero() {
		t.Fatalf("empty: %+v err=%v", r, err)
	}
	a, b, c3, d := newCapture(t, 1), newCapture(t, 2), newCapture(t, 3), newCapture(t, 4)
	for _, c := range []*capture.Capture{&a, &b, &c3, &d} {
		if err := s.Insert(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// a is done, b (head) waits two minutes, c3 waits one minute, d is due.
	a.Status, a.Step = capture.StatusDone, capture.StepDone
	b.Status, b.NextAttemptAt = capture.StatusWaiting, ts0().Add(2*time.Minute)
	c3.Status, c3.NextAttemptAt = capture.StatusWaiting, ts0().Add(time.Minute)
	for _, c := range []capture.Capture{a, b, c3} {
		if err := s.Update(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.NextDue(ctx, ts0(), capture.LaneMain)
	if err != nil || !res.OK || res.Capture.ID != d.ID {
		t.Fatalf("a waiting head must not block due d (%d): got %d ok=%v err=%v", d.ID, res.Capture.ID, res.OK, err)
	}

	d.Status, d.Step = capture.StatusDone, capture.StepDone
	if err = s.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	res, err = s.NextDue(ctx, ts0(), capture.LaneMain)
	if err != nil || res.OK {
		t.Fatalf("nothing due: got %d ok=%v err=%v", res.Capture.ID, res.OK, err)
	}
	if !res.WakeAt.Equal(c3.NextAttemptAt) || res.WakeAt.Location() != time.UTC {
		t.Fatalf("wakeAt=%v, want the earliest %v", res.WakeAt, c3.NextAttemptAt)
	}
	// Once both are due, the lowest ID goes first.
	res, err = s.NextDue(ctx, ts0().Add(2*time.Minute), capture.LaneMain)
	if err != nil || !res.OK || res.Capture.ID != b.ID {
		t.Fatalf("both due: want b (%d), got %d ok=%v err=%v", b.ID, res.Capture.ID, res.OK, err)
	}
}

// TestNextDueLanesMatchLaneOf proves the SQL lane predicate and
// capture.LaneOf agree: every capture is returned by exactly the lane LaneOf
// names, and the other lane sees nothing (not even a wake time).
func TestNextDueLanesMatchLaneOf(t *testing.T) {
	cases := []struct {
		kind capture.Kind
		step capture.Step
	}{
		{capture.KindAudio, capture.StepDownload},
		{capture.KindAudio, capture.StepExtract},
		{capture.KindAudio, capture.StepEnrich},
		{capture.KindText, capture.StepEnrich},
		{capture.KindText, capture.StepCleanup},
		{capture.KindFiles, capture.StepCollect},
		{capture.KindFiles, capture.StepDownload},
		{capture.KindFiles, capture.StepExtract},
		{capture.KindFiles, capture.StepEnrich},
		{capture.KindFiles, capture.StepWriteNote},
	}
	lanes := []capture.Lane{capture.LaneMain, capture.LaneDocuments}
	for _, tc := range cases {
		t.Run(string(tc.kind)+"/"+string(tc.step), func(t *testing.T) {
			ctx := context.Background()
			s := open(t, t.TempDir())
			c := newCapture(t, 1)
			c.Kind, c.Step = tc.kind, tc.step
			if err := s.Insert(ctx, &c); err != nil {
				t.Fatal(err)
			}
			want := capture.LaneOf(c)
			for _, lane := range lanes {
				due, err := s.NextDue(ctx, ts0(), lane)
				if err != nil {
					t.Fatal(err)
				}
				if got := due.OK && due.Capture.ID == c.ID; got != (lane == want) {
					t.Errorf("lane %s returned it = %v, LaneOf = %s", lane, got, want)
				}
				// Not due: only its own lane reports the wake time.
				due, err = s.NextDue(ctx, ts0().Add(-time.Hour), lane)
				if err != nil {
					t.Fatal(err)
				}
				if hasWake := !due.WakeAt.IsZero(); hasWake != (lane == want) {
					t.Errorf("lane %s wake = %v, LaneOf = %s", lane, due.WakeAt, want)
				}
			}
		})
	}
}

func TestNextDueUnknownLane(t *testing.T) {
	s := open(t, t.TempDir())
	if _, err := s.NextDue(context.Background(), ts0(), capture.Lane("bogus")); err == nil {
		t.Fatal("unknown lane must fail")
	}
}

func TestOldestUnfinished(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if _, ok, err := s.OldestUnfinished(ctx); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	a, b := newCapture(t, 1), newCapture(t, 2)
	for _, c := range []*capture.Capture{&a, &b} {
		if err := s.Insert(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	a.Status, a.Step = capture.StatusDone, capture.StepDone
	if err := s.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Not due yet must still count as unfinished.
	b.Status, b.NextAttemptAt = capture.StatusWaiting, ts0().Add(time.Hour)
	if err := s.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.OldestUnfinished(ctx)
	if err != nil || !ok || got.ID != b.ID {
		t.Fatalf("want b, got %d ok=%v err=%v", got.ID, ok, err)
	}
}

func TestPurgeDone(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	old, fresh, open1 := newCapture(t, 1), newCapture(t, 2), newCapture(t, 3)
	for _, c := range []*capture.Capture{&old, &fresh, &open1} {
		if err := s.Insert(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := ts0().Add(-30 * 24 * time.Hour)
	old.Status, old.Step, old.UpdatedAt = capture.StatusDone, capture.StepDone, cutoff.Add(-time.Second)
	fresh.Status, fresh.Step, fresh.UpdatedAt = capture.StatusDone, capture.StepDone, cutoff.Add(time.Second)
	open1.UpdatedAt = cutoff.Add(-time.Hour) // unfinished: never purged
	for _, c := range []capture.Capture{old, fresh, open1} {
		if err := s.Update(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PurgeDone(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("purged %d err=%v, want 1", n, err)
	}
	// The purged identity can be inserted again; the kept one cannot.
	dup := newCapture(t, 2)
	if err := s.Insert(ctx, &dup); !errors.Is(err, sqlite.ErrDuplicate) {
		t.Fatalf("fresh must still dedupe: %v", err)
	}
	again := newCapture(t, 1)
	if err := s.Insert(ctx, &again); err != nil {
		t.Fatalf("purged identity must be insertable: %v", err)
	}
}

func TestMigrationsIdempotentAndVersioned(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	c := newCapture(t, 1)
	if err := s.Insert(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2, err := sqlite.Open(dir) // re-applying must not fail or lose data
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if _, ok, _ := s2.OldestUnfinished(context.Background()); !ok {
		t.Fatal("row lost after reopen")
	}
	if v := s2.UserVersion(); v != 2 {
		t.Fatalf("user_version=%d, want 2", v)
	}
	if _, err := os.Stat(filepath.Join(dir, "voicenotes.db")); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"capture_files", "digest_runs", "digest_seen"} {
		if !s2.HasTable(tbl) {
			t.Fatalf("table %s missing", tbl)
		}
	}
}

func TestDurablePragmas(t *testing.T) {
	s := open(t, t.TempDir())
	if v := s.Synchronous(); v != 2 {
		t.Fatalf("synchronous=%d, want 2 (FULL): commits must fsync", v)
	}
}

func TestErrDuplicateReportsDuplicate(t *testing.T) {
	d, ok := errors.AsType[interface {
		error
		Duplicate() bool
	}](sqlite.ErrDuplicate)
	if !ok || !d.Duplicate() {
		t.Fatal("ErrDuplicate must expose Duplicate() true")
	}
}

func TestSetReplyMessageID(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newCapture(t, 1)
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReplyMessageID(ctx, c.ID, 77); err != nil {
		t.Fatal(err)
	}
	// A second reply never replaces the first one.
	if err := s.SetReplyMessageID(ctx, c.ID, 88); err != nil {
		t.Fatal(err)
	}
	d, err := s.NextDue(ctx, ts0(), capture.LaneMain)
	if err != nil || !d.OK {
		t.Fatalf("ok=%v err=%v", d.OK, err)
	}
	if d.Capture.ReplyMessageID != 77 {
		t.Fatalf("reply = %d, want 77", d.Capture.ReplyMessageID)
	}
	if err := s.SetReplyMessageID(ctx, 999, 1); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("unknown id: want ErrNotFound, got %v", err)
	}
}

// TestUpdateKeepsReplyMessageID covers the ingest/worker race: the worker
// may persist a copy read before ingest recorded the reply, and that stale
// zero must not erase it.
func TestUpdateKeepsReplyMessageID(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newCapture(t, 1)
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	stale := c
	if err := s.SetReplyMessageID(ctx, c.ID, 77); err != nil {
		t.Fatal(err)
	}
	stale.Step = capture.StepExtract
	if err := s.Update(ctx, stale); err != nil {
		t.Fatal(err)
	}
	d, err := s.NextDue(ctx, ts0(), capture.LaneMain)
	if err != nil || !d.OK {
		t.Fatalf("ok=%v err=%v", d.OK, err)
	}
	if d.Capture.ReplyMessageID != 77 || d.Capture.Step != capture.StepExtract {
		t.Fatalf("got reply %d step %s, want 77 extract", d.Capture.ReplyMessageID, d.Capture.Step)
	}
}

func TestDigestRunRecordAndLast(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if _, ok, err := s.LastDigestRun(ctx); err != nil || ok {
		t.Fatalf("empty LastDigestRun = ok %v err %v", ok, err)
	}

	first := digest.Run{LocalDate: "2026-10-29", Status: digest.StatusSent, SentAt: ts0(), NoteCount: 3}
	second := digest.Run{LocalDate: "2026-10-30", Status: digest.StatusEmpty, SentAt: ts0().Add(time.Hour)}
	for _, r := range []digest.Run{second, first} {
		if err := s.RecordDigestRun(ctx, r); err != nil {
			t.Fatalf("RecordDigestRun: %v", err)
		}
	}

	got, ok, err := s.LastDigestRun(ctx)
	if err != nil || !ok {
		t.Fatalf("LastDigestRun = ok %v err %v", ok, err)
	}
	if !reflect.DeepEqual(got, second) {
		t.Errorf("LastDigestRun = %+v, want %+v", got, second)
	}
}

func TestLastSentDigestDate(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if d, ok, err := s.LastSentDigestDate(ctx); err != nil || ok || d != "" {
		t.Fatalf("empty LastSentDigestDate = %q ok %v err %v", d, ok, err)
	}
	runs := []digest.Run{
		{LocalDate: "2026-10-28", Status: digest.StatusSent, SentAt: ts0()},
		{LocalDate: "2026-10-29", Status: digest.StatusSent, SentAt: ts0()},
		{LocalDate: "2026-10-30", Status: digest.StatusEmpty, SentAt: ts0()},
		{LocalDate: "2026-10-31", Status: digest.StatusSkipped, SentAt: ts0()},
	}
	for _, r := range runs {
		if err := s.RecordDigestRun(ctx, r); err != nil {
			t.Fatalf("RecordDigestRun: %v", err)
		}
	}
	if d, ok, err := s.LastSentDigestDate(ctx); err != nil || !ok || d != "2026-10-29" {
		t.Errorf("LastSentDigestDate = %q ok %v err %v, want 2026-10-29", d, ok, err)
	}
}

func TestRecordDigestRunKeepsFirstPerDay(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	first := digest.Run{LocalDate: "2026-10-30", Status: digest.StatusSent, SentAt: ts0(), NoteCount: 2}
	again := digest.Run{LocalDate: "2026-10-30", Status: digest.StatusSkipped, SentAt: ts0().Add(time.Hour)}
	for _, r := range []digest.Run{first, again} {
		if err := s.RecordDigestRun(ctx, r); err != nil {
			t.Fatalf("RecordDigestRun: %v", err)
		}
	}

	got, _, err := s.LastDigestRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Errorf("LastDigestRun = %+v, want the first record %+v", got, first)
	}
}

func TestMarkSeenInsertsKeepsFirstShownAndPrunes(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	listed, inbox := []string{"memos/a", "memos/b"}, []string{"memos/a", "memos/b"}
	if err := s.MarkSeen(ctx, listed, "2026-10-29", inbox); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	// b left the inbox, c is new, a is listed again.
	listed, inbox = []string{"memos/a", "memos/c"}, []string{"memos/a", "memos/c"}
	if err := s.MarkSeen(ctx, listed, "2026-10-30", inbox); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	got, err := s.SeenNotes(ctx)
	if err != nil {
		t.Fatalf("SeenNotes: %v", err)
	}
	want := map[string]string{"memos/a": "2026-10-29", "memos/c": "2026-10-30"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SeenNotes = %v, want %v", got, want)
	}
}

func TestMarkSeenEmptyInboxClearsAll(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if err := s.MarkSeen(ctx, []string{"memos/a"}, "2026-10-29", []string{"memos/a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSeen(ctx, nil, "2026-10-30", nil); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	got, err := s.SeenNotes(ctx)
	if err != nil || len(got) != 0 {
		t.Errorf("SeenNotes = %v, %v; want empty", got, err)
	}
}

func TestCountUnfinished(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if n, err := s.CountUnfinished(ctx); err != nil || n != 0 {
		t.Fatalf("empty: n=%d err=%v", n, err)
	}
	a, b, c := newCapture(t, 1), newCapture(t, 2), newCapture(t, 3)
	for _, x := range []*capture.Capture{&a, &b, &c} {
		if err := s.Insert(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	a.Status, a.Step = capture.StatusDone, capture.StepDone
	if err := s.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountUnfinished(ctx); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v, want 2", n, err)
	}
}

func TestFilePaths(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	a, b, none := newCapture(t, 1), newCapture(t, 2), newCapture(t, 3)
	a.Files[0].LocalPath, b.Files[0].LocalPath = "tg--1001234567890-1-42.ogg", "2.ogg"
	b.Status, b.Step = capture.StatusDone, capture.StepDone // done rows still count
	for _, c := range []*capture.Capture{&a, &b, &none} {
		if err := s.Insert(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.FilePaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tg--1001234567890-1-42.ogg", "2.ogg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilePaths = %v, want %v", got, want)
	}
}

// newBatch returns a files capture at collect holding n items, one message
// each, starting at message msgID.
func newBatch(t *testing.T, msgID int64, n int) capture.Capture {
	t.Helper()

	c, err := capture.New(capture.KindFiles, capture.StepCollect, ts0())
	if err != nil {
		t.Fatal(err)
	}
	c.Source, c.SourceChatID, c.SourceMessageID = "telegram", -1001234567890, msgID
	c.AlbumID = "album-1"
	for i := range n {
		c.Files = append(c.Files, capture.File{
			Position: i, Source: c.Source, SourceChatID: c.SourceChatID, SourceMessageID: msgID + int64(i),
			Kind: capture.FileDocument, FileID: "FAKE-DOC", MimeType: "application/pdf",
			FileName: "scan.pdf", FileSize: 1024, State: capture.FilePending,
		})
	}

	return c
}

func TestInsertRoundTripsFiles(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newBatch(t, 10, 3)
	c.Files[1].Kind, c.Files[1].MimeType, c.Files[1].FileName = capture.FileImage, "image/jpeg", ""
	c.Files[2].LocalPath, c.Files[2].State, c.Files[2].Failure = "f.pdf", capture.FileFailed, "too large"
	c.Files[2].Text, c.Files[2].TextSource, c.Files[2].Language = "body", capture.TextDocling, "en"
	c.Files[2].Timeouts, c.Files[2].Attached, c.Files[2].AttachFailure = 2, true, "rejected"
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	for i, f := range c.Files {
		if f.ID == 0 || f.CaptureID != c.ID {
			t.Fatalf("file %d: ID %d CaptureID %d (capture %d) not assigned", i, f.ID, f.CaptureID, c.ID)
		}
	}
	got, ok, err := s.OldestUnfinished(ctx)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip mismatch\n got %+v\nwant %+v", got, c)
	}
}

func TestTextCaptureHasNoFiles(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c, err := capture.New(capture.KindText, capture.StepEnrich, ts0())
	if err != nil {
		t.Fatal(err)
	}
	c.Source, c.SourceChatID, c.SourceMessageID, c.Text = "telegram", -1001234567890, 1, "hi"
	if err = s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.OldestUnfinished(ctx)
	if err != nil || got.Files != nil {
		t.Fatalf("files = %#v err=%v, want nil", got.Files, err)
	}
}

func TestInsertDuplicateFileIdentityLeavesNoCapture(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	a := newBatch(t, 10, 2) // items 10, 11
	if err := s.Insert(ctx, &a); err != nil {
		t.Fatal(err)
	}
	// A new capture (message 11) whose item 11 was already stored.
	b := newBatch(t, 11, 1)
	if err := s.Insert(ctx, &b); !errors.Is(err, sqlite.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	if n, err := s.CountUnfinished(ctx); err != nil || n != 1 {
		t.Fatalf("unfinished = %d err=%v: the failed insert left a capture behind", n, err)
	}
}

func TestUpdatePersistsMutableFileColumns(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newBatch(t, 10, 2)
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	f := &c.Files[1]
	f.FileID, f.MimeType, f.FileName, f.FileSize = "", "application/x-pdf", "", 4096
	f.LocalPath, f.State, f.Text, f.TextSource = "tg-10-11.pdf", capture.FileDone, "text", capture.TextVision
	f.Language, f.Failure, f.Timeouts = "de", "slow", 3
	f.Attached, f.AttachFailure = true, "refused"
	c.Step, c.AlbumID = capture.StepExtract, "album-2"
	if err := s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.OldestUnfinished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("mismatch\n got %+v\nwant %+v", got, c)
	}

	// Identity, kind and position are fixed at ingest.
	moved := c
	moved.Files = slices.Clone(c.Files)
	moved.Files[1].Kind, moved.Files[1].Position, moved.Files[1].SourceMessageID = capture.FileVideo, 7, 99
	if err := s.Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = s.OldestUnfinished(ctx); !reflect.DeepEqual(got, c) {
		t.Fatalf("immutable file columns changed\n got %+v\nwant %+v", got.Files[1], c.Files[1])
	}
}

func TestUpdateRejectsUnknownFile(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newBatch(t, 10, 1)
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	c.Files = append(c.Files, capture.File{Position: 1, Kind: capture.FileImage, State: capture.FilePending})
	c.Step = capture.StepDownload
	if err := s.Update(ctx, c); err == nil {
		t.Fatal("a file never inserted must not be silently dropped")
	}
	// The failed update changed nothing.
	got, _, err := s.OldestUnfinished(ctx)
	if err != nil || got.Step != capture.StepCollect || len(got.Files) != 1 {
		t.Fatalf("got step %s with %d files err=%v, want the stored capture", got.Step, len(got.Files), err)
	}
}

func TestPurgeDoneCascadesFiles(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	done, kept := newBatch(t, 10, 2), newBatch(t, 20, 1)
	done.Files[0].LocalPath, kept.Files[0].LocalPath = "gone.pdf", "kept.pdf"
	for _, c := range []*capture.Capture{&done, &kept} {
		if err := s.Insert(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	done.Status, done.Step, done.UpdatedAt = capture.StatusDone, capture.StepDone, ts0().Add(-time.Hour)
	if err := s.Update(ctx, done); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeDone(ctx, ts0()); err != nil || n != 1 {
		t.Fatalf("purged %d err=%v, want 1", n, err)
	}
	if n := s.CountFiles(); n != 1 {
		t.Fatalf("capture_files rows = %d, want 1 (cascade)", n)
	}
	if got, err := s.FilePaths(ctx); err != nil || !slices.Equal(got, []string{"kept.pdf"}) {
		t.Fatalf("FilePaths = %v err=%v", got, err)
	}
	// The purged items' identities are free again.
	again := newBatch(t, 11, 1)
	if err := s.Insert(ctx, &again); err != nil {
		t.Fatalf("purged item identity must be insertable: %v", err)
	}
}
