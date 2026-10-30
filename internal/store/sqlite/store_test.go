package sqlite_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
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
	c.FileID = "FAKE-FILE"

	return c
}

func TestInsertAssignsIDAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newCapture(t, 1)
	c.FileSize, c.MimeType = 2048, "audio/ogg"
	if err := s.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if c.ID == 0 {
		t.Fatal("ID not assigned")
	}
	d0, err := s.NextDue(ctx, ts0())
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
	c.AudioPath, c.Text, c.Language = "a/1.ogg", "hello", "en"
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
	d0, err := s2.NextDue(ctx, ts0().Add(time.Hour))
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

func TestNextDueIsStrictFIFO(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if r, err := s.NextDue(ctx, ts0()); err != nil || r.OK || !r.WakeAt.IsZero() {
		t.Fatalf("empty: %+v err=%v", r, err)
	}
	a, b, c3 := newCapture(t, 1), newCapture(t, 2), newCapture(t, 3)
	for _, c := range []*capture.Capture{&a, &b, &c3} {
		if err := s.Insert(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// a is done, b (head) waits until later, d is pending and due.
	a.Status, a.Step = capture.StatusDone, capture.StepDone
	b.Status, b.NextAttemptAt = capture.StatusWaiting, ts0().Add(time.Minute)
	for _, c := range []capture.Capture{a, b} {
		if err := s.Update(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.NextDue(ctx, ts0())
	got, ok, wake := res.Capture, res.OK, res.WakeAt
	if err != nil || ok {
		t.Fatalf("waiting head must block later captures: got %d ok=%v err=%v", got.ID, ok, err)
	}
	if !wake.Equal(b.NextAttemptAt) || wake.Location() != time.UTC {
		t.Fatalf("wakeAt=%v, want %v", wake, b.NextAttemptAt)
	}
	res, err = s.NextDue(ctx, ts0().Add(time.Minute))
	got, ok = res.Capture, res.OK
	if err != nil || !ok || got.ID != b.ID {
		t.Fatalf("after backoff want head b (%d), got %d ok=%v err=%v", b.ID, got.ID, ok, err)
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
	if v := s2.UserVersion(); v != 1 {
		t.Fatalf("user_version=%d, want 1", v)
	}
	if _, err := os.Stat(filepath.Join(dir, "voicenotes.db")); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"digest_runs", "digest_seen"} {
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
	d, err := s.NextDue(ctx, ts0())
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
	stale.Step = capture.StepTranscribe
	if err := s.Update(ctx, stale); err != nil {
		t.Fatal(err)
	}
	d, err := s.NextDue(ctx, ts0())
	if err != nil || !d.OK {
		t.Fatalf("ok=%v err=%v", d.OK, err)
	}
	if d.Capture.ReplyMessageID != 77 || d.Capture.Step != capture.StepTranscribe {
		t.Fatalf("got reply %d step %s, want 77 transcribe", d.Capture.ReplyMessageID, d.Capture.Step)
	}
}
