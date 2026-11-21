package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

const micChat = int64(-1001234567890)

// fakeRecordings is a RecordingInserter that records rows and may fail.
type fakeRecordings struct {
	nextID int64
	rows   []capture.Capture
	err    error
	// reply is the start reply the real store would attach.
	reply int64
}

func (f *fakeRecordings) InsertRecording(_ context.Context, c *capture.Capture) error {
	if f.err != nil {
		return f.err
	}
	f.nextID++
	c.ID = f.nextID
	c.ReplyMessageID = f.reply
	f.rows = append(f.rows, *c)

	return nil
}

func newRecordingIngester(t *testing.T, rec *fakeRecordings, w *fakeWaker) *pipeline.Ingester {
	t.Helper()
	in, err := pipeline.NewIngester(&fakeInserter{}, w, pipeline.WithRecordings(rec, micChat))
	if err != nil {
		t.Fatal(err)
	}

	return in
}

func recStart() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }

func TestIngestRecordingStored(t *testing.T) {
	w := &fakeWaker{}
	rec := &fakeRecordings{}
	in := newRecordingIngester(t, rec, w)

	meta := pipeline.RecordingMeta{ID: 1700000000123, StartedAt: recStart(), EndReason: "tap", Recorder: "desk"}
	res, err := in.IngestRecording(context.Background(), meta, "mic-1.wav")
	if err != nil || res != pipeline.Stored {
		t.Fatalf("res %v err %v", res, err)
	}
	if len(rec.rows) != 1 || w.n != 1 {
		t.Fatalf("rows %d wakes %d", len(rec.rows), w.n)
	}
	c := rec.rows[0]
	if c.Kind != capture.KindAudio || c.Step != capture.StepExtract || !c.ReceivedAt.Equal(recStart()) ||
		c.Source != capture.SourceMic || c.SourceChatID != micChat || c.SourceMessageID != meta.ID {
		t.Errorf("capture %+v", c)
	}
	if len(c.Files) != 1 {
		t.Fatalf("files %+v", c.Files)
	}
	f := c.Files[0]
	if f.Kind != capture.FileAudio || f.MimeType != "audio/wav" || f.LocalPath != "mic-1.wav" ||
		f.Source != capture.SourceMic || f.SourceChatID != micChat || f.SourceMessageID != meta.ID {
		t.Errorf("file %+v", f)
	}
}

func TestIngestRecordingDuplicate(t *testing.T) {
	w := &fakeWaker{}
	in := newRecordingIngester(t, &fakeRecordings{err: dupError{}}, w)

	res, err := in.IngestRecording(context.Background(), pipeline.RecordingMeta{ID: 1, StartedAt: recStart()}, "a.wav")
	if err != nil || res != pipeline.Duplicate {
		t.Fatalf("res %v err %v", res, err)
	}
	if w.n != 0 {
		t.Errorf("duplicate woke %d", w.n)
	}
}

func TestIngestRecordingInsertFailure(t *testing.T) {
	w := &fakeWaker{}
	in := newRecordingIngester(t, &fakeRecordings{err: errStore}, w)

	res, err := in.IngestRecording(context.Background(), pipeline.RecordingMeta{ID: 1, StartedAt: recStart()}, "a.wav")
	if !errors.Is(err, errStore) || res == pipeline.Stored || res == pipeline.Duplicate {
		t.Fatalf("res %v err %v", res, err)
	}
	if w.n != 0 {
		t.Errorf("failure woke %d", w.n)
	}
}

func TestIngestRecordingNotEnabled(t *testing.T) {
	in, err := pipeline.NewIngester(&fakeInserter{}, &fakeWaker{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = in.IngestRecording(context.Background(), pipeline.RecordingMeta{ID: 1, StartedAt: recStart()}, "a.wav")
	if !errors.Is(err, pipeline.ErrNoRecordings) {
		t.Fatalf("err %v", err)
	}
}

// TestIngestRecordingToNote runs a stored recording through the real extract
// and write_note handlers: the note carries the WAV and the recording time.
func TestIngestRecordingToNote(t *testing.T) {
	rec := &fakeRecordings{reply: 555}
	in := newRecordingIngester(t, rec, &fakeWaker{})
	_, err := in.IngestRecording(context.Background(), pipeline.RecordingMeta{ID: 9, StartedAt: recStart()}, "1.wav")
	if err != nil {
		t.Fatal(err)
	}
	c := rec.rows[0]

	r := newRig(t)
	r.audio.files["1.wav"] = []byte("RIFF-WAV")
	r.tr.res = pipeline.TranscribeResult{Text: "remember the milk"}

	c, out := r.run(c.Step, c)
	if out.Class != capture.ClassOK {
		t.Fatalf("extract %+v", out)
	}
	if r.tr.req.Ext != ".wav" || string(r.tr.req.Audio) != "RIFF-WAV" {
		t.Errorf("transcribe request %+v", r.tr.req)
	}
	c, out = r.run(capture.StepWriteNote, c)
	if out.Class != capture.ClassOK {
		t.Fatalf("write_note %+v", out)
	}
	if len(r.notes.created) != 1 || !r.notes.created[0].ReceivedAt.Equal(recStart()) {
		t.Fatalf("created %+v", r.notes.created)
	}
	a := r.notes.attached
	if len(a) != 1 || a[0].mime != "audio/wav" || string(a[0].data) != "RIFF-WAV" ||
		a[0].filename != "voice-2026-03-04.wav" {
		t.Errorf("attached %+v", a)
	}
	if c.ReplyMessageID != 555 {
		t.Errorf("reply %d", c.ReplyMessageID)
	}
}

// TestIngestRecordingDueAtServiceClock: a recorder clock ahead of the service
// keeps the note time but must not delay processing (vn-c82.8).
func TestIngestRecordingDueAtServiceClock(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	rec := &fakeRecordings{}
	in, err := pipeline.NewIngester(&fakeInserter{}, &fakeWaker{},
		pipeline.WithRecordings(rec, micChat), pipeline.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}

	started := now.Add(10 * time.Minute)
	meta := pipeline.RecordingMeta{ID: 1, StartedAt: started}
	if _, err := in.IngestRecording(context.Background(), meta, "a.wav"); err != nil {
		t.Fatal(err)
	}
	c := rec.rows[0]
	if !c.ReceivedAt.Equal(started) {
		t.Errorf("note time = %v, want %v", c.ReceivedAt, started)
	}
	if c.NextAttemptAt.After(now) || !c.UpdatedAt.Equal(now) {
		t.Errorf("next attempt %v, updated %v, want due at service now %v", c.NextAttemptAt, c.UpdatedAt, now)
	}
}
