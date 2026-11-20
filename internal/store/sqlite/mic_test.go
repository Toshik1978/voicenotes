package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
)

const micChat = int64(-1001234567890)

func newRecording(t *testing.T, id int64) capture.Capture {
	t.Helper()

	c, err := capture.New(capture.KindAudio, capture.StepExtract, ts0())
	if err != nil {
		t.Fatal(err)
	}
	c.Source = capture.SourceMic
	c.SourceChatID = micChat
	c.SourceMessageID = id
	c.Files = []capture.File{{
		Source: c.Source, SourceChatID: c.SourceChatID, SourceMessageID: id,
		Kind: capture.FileAudio, State: capture.FilePending, MimeType: "audio/wav",
		LocalPath: "mic-1.wav",
	}}

	return c
}

func TestInsertRecordingTakesStartReply(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if err := s.PutStartReply(ctx, 42, micChat, 555); err != nil {
		t.Fatal(err)
	}

	c := newRecording(t, 42)
	if err := s.InsertRecording(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if c.ReplyMessageID != 555 {
		t.Errorf("struct ReplyMessageID = %d, want 555", c.ReplyMessageID)
	}
	got, ok, err := s.Get(c.ID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.ReplyMessageID != 555 || got.Source != capture.SourceMic {
		t.Errorf("stored capture: reply %d source %q", got.ReplyMessageID, got.Source)
	}
	if _, _, ok, err = s.TakeStartReply(ctx, 42); err != nil || ok {
		t.Errorf("reply row must be consumed: ok=%v err=%v", ok, err)
	}
}

func TestInsertRecordingWithoutStartReply(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newRecording(t, 43)
	if err := s.InsertRecording(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if c.ReplyMessageID != 0 || c.ID == 0 {
		t.Errorf("ReplyMessageID %d ID %d", c.ReplyMessageID, c.ID)
	}
}

func TestInsertRecordingDuplicateKeepsStartReply(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	first := newRecording(t, 44)
	if err := s.InsertRecording(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStartReply(ctx, 44, micChat, 777); err != nil {
		t.Fatal(err)
	}
	dup := newRecording(t, 44)
	if err := s.InsertRecording(ctx, &dup); !errors.Is(err, sqlite.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	if dup.ReplyMessageID != 0 {
		t.Errorf("duplicate must not receive the reply, got %d", dup.ReplyMessageID)
	}
	chat, msg, ok, err := s.TakeStartReply(ctx, 44)
	if err != nil || !ok || chat != micChat || msg != 777 {
		t.Errorf("reply row consumed by a duplicate: %d %d %v %v", chat, msg, ok, err)
	}
}

func TestStartReplyPutTakeAndStale(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	now := ts0()
	s.SetNow(func() time.Time { return now })

	if err := s.PutStartReply(ctx, 1, micChat, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStartReply(ctx, 1, micChat, 99); err != nil { // repeat keeps the first
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if err := s.PutStartReply(ctx, 2, micChat, 20); err != nil {
		t.Fatal(err)
	}

	stale, err := s.StaleStartReplies(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].RecordingID != 1 || stale[0].MessageID != 10 ||
		stale[0].ChatID != micChat || !stale[0].CreatedAt.Equal(ts0()) {
		t.Fatalf("stale = %+v", stale)
	}

	chat, msg, ok, err := s.TakeStartReply(ctx, 1)
	if err != nil || !ok || chat != micChat || msg != 10 {
		t.Fatalf("take: %d %d %v %v", chat, msg, ok, err)
	}
	if _, _, ok, _ = s.TakeStartReply(ctx, 1); ok {
		t.Error("second take must find nothing")
	}
	if stale, err = s.StaleStartReplies(ctx, now.Add(time.Hour)); err != nil || len(stale) != 1 {
		t.Errorf("remaining stale = %+v err %v", stale, err)
	}
}

func TestRecorders(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if err := s.UpsertRecorder(ctx, "desk", ts0(), true, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRecorderAlert(ctx, "desk", "silent"); err != nil {
		t.Fatal(err)
	}
	later := ts0().Add(time.Minute)
	if err := s.UpsertRecorder(ctx, "desk", later, false, true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRecorder(ctx, "kitchen", ts0(), true, true); err != nil {
		t.Fatal(err)
	}

	recs, err := s.Recorders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("recorders = %+v", recs)
	}
	desk, kitchen := recs[0], recs[1]
	if desk.Name != "desk" || !desk.LastSeen.Equal(later) || desk.ReceiverPresent || !desk.StorageFull ||
		desk.Alert != "silent" {
		t.Errorf("desk = %+v (upsert must keep the alert)", desk)
	}
	if kitchen.Name != "kitchen" || kitchen.Alert != "ok" || !kitchen.ReceiverPresent {
		t.Errorf("kitchen = %+v", kitchen)
	}
	if err = s.SetRecorderAlert(ctx, "nobody", "ok"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Errorf("unknown recorder: got %v", err)
	}
}

func TestNoSpeechPersists(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	c := newRecording(t, 50)
	c.NoSpeech = true
	if err := s.InsertRecording(ctx, &c); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get(c.ID)
	if !got.NoSpeech {
		t.Error("NoSpeech lost on insert")
	}

	n := newRecording(t, 51)
	if err := s.Insert(ctx, &n); err != nil {
		t.Fatal(err)
	}
	n.NoSpeech = true
	if err := s.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = s.Get(n.ID); !got.NoSpeech {
		t.Error("NoSpeech lost on update")
	}
}
