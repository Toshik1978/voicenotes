package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

var errNet = errors.New("network down")

type fakeAudio struct {
	files    map[string][]byte
	writeErr error
	events   *[]string
}

func newFakeAudio() *fakeAudio { return &fakeAudio{files: map[string][]byte{}} }

func (f *fakeAudio) Write(name string, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[name] = data
	if f.events != nil {
		*f.events = append(*f.events, "write")
	}

	return nil
}

func (f *fakeAudio) Read(name string) ([]byte, error) {
	d, ok := f.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return d, nil
}

func (f *fakeAudio) Exists(name string) (bool, error) { _, ok := f.files[name]; return ok, nil }

func (f *fakeAudio) Remove(name string) error { delete(f.files, name); return nil }

type fakeDownloader struct {
	data  []byte
	err   error
	calls int
}

func (f *fakeDownloader) Download(context.Context, string) ([]byte, error) {
	f.calls++
	return f.data, f.err
}

type sendCall struct {
	chat, replyTo, msg int64
	text               string
}

type fakeReplier struct {
	sends, edits []sendCall
	sendErr      error
	editErr      error
	nextID       int64
	editID       int64 // returned by Edit when non-zero
	events       *[]string
}

func (f *fakeReplier) Send(_ context.Context, chat, replyTo int64, text string) (int64, error) {
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sends = append(f.sends, sendCall{chat: chat, replyTo: replyTo, text: text})
	if f.events != nil {
		*f.events = append(*f.events, "send")
	}

	return f.nextID, nil
}

func (f *fakeReplier) Edit(_ context.Context, chat, msg int64, text string) (int64, error) {
	if f.editErr != nil {
		return 0, f.editErr
	}
	f.edits = append(f.edits, sendCall{chat: chat, msg: msg, text: text})
	if f.editID != 0 {
		return f.editID, nil
	}

	return msg, nil
}

type fakeTranscriber struct {
	req  pipeline.TranscribeRequest
	res  pipeline.TranscribeResult
	out  capture.Outcome
	call int
}

func (f *fakeTranscriber) Transcribe(
	_ context.Context,
	r pipeline.TranscribeRequest,
) (pipeline.TranscribeResult, capture.Outcome) {
	f.req = r
	f.call++
	return f.res, f.out
}

type attachCall struct {
	name, filename, mime string
	data                 []byte
}

type fakeNotes struct {
	created   []pipeline.NoteInput
	attached  []attachCall
	createOut capture.Outcome
	attachOut capture.Outcome
}

func (f *fakeNotes) Create(_ context.Context, n pipeline.NoteInput) (string, string, capture.Outcome) {
	f.created = append(f.created, n)
	if f.createOut.Class != capture.ClassOK {
		return "", "", f.createOut
	}
	return "memos/abc", "https://memos.example.org/memos/abc?a=1&b=2", capture.OK()
}

func (f *fakeNotes) Attach(_ context.Context, name, filename, mime string, data []byte) capture.Outcome {
	f.attached = append(f.attached, attachCall{name, filename, mime, data})
	return f.attachOut
}

type rig struct {
	audio *fakeAudio
	dl    *fakeDownloader
	rep   *fakeReplier
	tr    *fakeTranscriber
	notes *fakeNotes
	h     map[capture.Step]pipeline.Handler
}

func newRig(t *testing.T, langs ...string) *rig {
	t.Helper()

	return newRigWith(
		t,
		pipeline.StepsConfig{InboxTag: "inbox", TodoTag: "todo", NoteLimit: 1000, SourceLanguages: langs},
	)
}

func newRigWith(t *testing.T, cfg pipeline.StepsConfig) *rig {
	t.Helper()
	r := &rig{
		audio: newFakeAudio(), dl: &fakeDownloader{data: []byte("OGG")},
		rep: &fakeReplier{nextID: 77}, tr: &fakeTranscriber{out: capture.OK()}, notes: &fakeNotes{},
	}
	h, err := pipeline.NewHandlers(pipeline.Deps{
		Transcriber: r.tr, Notes: r.notes, Replier: r.rep, Downloader: r.dl, Audio: r.audio,
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.h = h

	return r
}

func (r *rig) run(step capture.Step, c capture.Capture) (capture.Capture, capture.Outcome) {
	return r.h[step](context.Background(), c)
}

func base() capture.Capture {
	return capture.Capture{
		ID: 5, SourceChatID: 10, SourceMessageID: 20, FileID: "F", Kind: capture.KindAudio,
		ReceivedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestNewHandlersValidates(t *testing.T) {
	if _, err := pipeline.NewHandlers(pipeline.Deps{}, pipeline.StepsConfig{}); err == nil {
		t.Fatal("want error for missing deps")
	}
	r := newRig(t)
	for _, s := range []capture.Step{
		capture.StepDownload, capture.StepTranscribe, capture.StepEnrich,
		capture.StepWriteNote, capture.StepFinalizeReply, capture.StepCleanup,
	} {
		if r.h[s] == nil {
			t.Errorf("no handler for %s", s)
		}
	}
}

func TestDownload(t *testing.T) {
	t.Run("writes audio then sends hourglass", func(t *testing.T) {
		r := newRig(t)
		var events []string
		r.audio.events, r.rep.events = &events, &events
		got, out := r.run(capture.StepDownload, base())
		if out.Class != capture.ClassOK {
			t.Fatalf("outcome %v", out)
		}
		if got.AudioPath != "5.ogg" || string(r.audio.files["5.ogg"]) != "OGG" {
			t.Errorf("audio path %q files %v", got.AudioPath, r.audio.files)
		}
		if got.ReplyMessageID != 77 {
			t.Errorf("reply id %d", got.ReplyMessageID)
		}
		if len(r.rep.sends) != 1 || r.rep.sends[0].text != "⏳" || r.rep.sends[0].chat != 10 ||
			r.rep.sends[0].replyTo != 20 {
			t.Errorf("sends %+v", r.rep.sends)
		}
		if strings.Join(events, ",") != "write,send" {
			t.Errorf("order %v", events)
		}
	})
	t.Run("mime picks extension", func(t *testing.T) {
		r := newRig(t)
		c := base()
		c.MimeType = "audio/mpeg"
		got, _ := r.run(capture.StepDownload, c)
		if got.AudioPath != "5.mp3" {
			t.Errorf("path %q", got.AudioPath)
		}
	})
	t.Run("existing reply is not resent", func(t *testing.T) {
		r := newRig(t)
		c := base()
		c.ReplyMessageID = 9
		got, out := r.run(capture.StepDownload, c)
		if out.Class != capture.ClassOK || len(r.rep.sends) != 0 || got.ReplyMessageID != 9 {
			t.Errorf("out %v sends %v id %d", out, r.rep.sends, got.ReplyMessageID)
		}
	})
	t.Run("rerun with file present skips download", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("OGG")
		c := base()
		c.AudioPath = "5.ogg"
		_, out := r.run(capture.StepDownload, c)
		if out.Class != capture.ClassOK || r.dl.calls != 0 || len(r.rep.sends) != 1 {
			t.Errorf("out %v calls %d sends %d", out, r.dl.calls, len(r.rep.sends))
		}
	})
}

func TestDownloadFailures(t *testing.T) {
	t.Run("download error is temporary on telegram", func(t *testing.T) {
		r := newRig(t)
		r.dl.err = errNet
		_, out := r.run(capture.StepDownload, base())
		if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingTelegram {
			t.Errorf("out %+v", out)
		}
		if len(r.rep.sends) != 0 {
			t.Error("must not acknowledge before local persistence")
		}
	})
	t.Run("file too big is permanent", func(t *testing.T) {
		r := newRig(t)
		r.dl.err = fmt.Errorf("wrapped: %w", pipeline.ErrFileTooLarge)
		_, out := r.run(capture.StepDownload, base())
		if out.Class != capture.ClassPermanent || out.Reason == "" {
			t.Errorf("out %+v", out)
		}
	})
	t.Run("rejected file is permanent", func(t *testing.T) {
		r := newRig(t)
		r.dl.err = fmt.Errorf("telegram: %w", pipeline.ErrRejected)
		_, out := r.run(capture.StepDownload, base())
		if out.Class != capture.ClassPermanent || out.Reason == "" {
			t.Errorf("out %+v", out)
		}
	})
	t.Run("write error is temporary and sends nothing", func(t *testing.T) {
		r := newRig(t)
		r.audio.writeErr = errNet
		got, out := r.run(capture.StepDownload, base())
		if out.Class != capture.ClassTemporary || len(r.rep.sends) != 0 || got.AudioPath != "" {
			t.Errorf("out %+v sends %v path %q", out, r.rep.sends, got.AudioPath)
		}
	})
	t.Run("failed hourglass does not fail the step", func(t *testing.T) {
		r := newRig(t)
		r.rep.sendErr = errNet
		got, out := r.run(capture.StepDownload, base())
		if out.Class != capture.ClassOK || got.AudioPath != "5.ogg" || got.ReplyMessageID != 0 {
			t.Errorf("out %+v got %+v", out, got)
		}
	})
}

func TestTranscribe(t *testing.T) {
	withAudio := func() capture.Capture { c := base(); c.AudioPath = "5.ogg"; return c }
	tests := []struct {
		name      string
		langs     []string
		files     map[string][]byte
		res       pipeline.TranscribeResult
		out       capture.Outcome
		wantClass capture.Class
		wantText  string
		wantLang  string // sent
	}{
		{
			"ok no language",
			nil,
			map[string][]byte{"5.ogg": []byte("A")},
			pipeline.TranscribeResult{Text: "hi", Language: "en"},
			capture.OK(),
			capture.ClassOK,
			"hi",
			"",
		},
		{
			"single language passed",
			[]string{"ru"},
			map[string][]byte{"5.ogg": []byte("A")},
			pipeline.TranscribeResult{Text: "x"},
			capture.OK(),
			capture.ClassOK,
			"x",
			"ru",
		},
		{
			"two languages not passed",
			[]string{"ru", "en"},
			map[string][]byte{"5.ogg": []byte("A")},
			pipeline.TranscribeResult{Text: "x"},
			capture.OK(),
			capture.ClassOK,
			"x",
			"",
		},
		{
			"empty transcript is ok",
			nil,
			map[string][]byte{"5.ogg": []byte("A")},
			pipeline.TranscribeResult{},
			capture.OK(),
			capture.ClassOK,
			"",
			"",
		},
		{
			"temporary passes through",
			nil,
			map[string][]byte{"5.ogg": []byte("A")},
			pipeline.TranscribeResult{},
			capture.Temporary("http 503").On(capture.WaitingSTT),
			capture.ClassTemporary,
			"",
			"",
		},
		{
			"permanent passes through",
			nil,
			map[string][]byte{"5.ogg": []byte("A")},
			pipeline.TranscribeResult{},
			capture.Permanent("http 415").On(capture.WaitingSTT),
			capture.ClassPermanent,
			"",
			"",
		},
		{
			"missing file is permanent",
			nil,
			nil,
			pipeline.TranscribeResult{},
			capture.OK(),
			capture.ClassPermanent,
			"",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, tt.langs...)
			r.audio.files = map[string][]byte{}
			maps.Copy(r.audio.files, tt.files)
			r.tr.res, r.tr.out = tt.res, tt.out
			got, out := r.run(capture.StepTranscribe, withAudio())
			if out.Class != tt.wantClass {
				t.Fatalf("class %v want %v", out.Class, tt.wantClass)
			}
			if tt.wantClass != capture.ClassOK {
				return
			}
			if got.Text != tt.wantText {
				t.Errorf("text %q", got.Text)
			}
			if r.tr.req.Language != tt.wantLang || r.tr.req.ID != "5" || string(r.tr.req.Audio) != "A" {
				t.Errorf("req %+v", r.tr.req)
			}
			if got.Language != tt.res.Language {
				t.Errorf("language %q", got.Language)
			}
		})
	}
}

func TestEnrich(t *testing.T) {
	r := newRig(t)
	c := base()
	c.Text = "hello"
	got, out := r.run(capture.StepEnrich, c)
	if out.Class != capture.ClassOK || got.LLMSkipped != capture.SkippedDisabled || got.Text != "hello" {
		t.Errorf("got %+v out %+v", got, out)
	}
}

func TestWriteNote(t *testing.T) {
	c := base()
	c.AudioPath, c.Text = "5.ogg", "buy milk"
	t.Run("create then attach", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("AUDIO")
		got, out := r.run(capture.StepWriteNote, c)
		if out.Class != capture.ClassOK {
			t.Fatalf("out %+v", out)
		}
		if got.NoteName != "memos/abc" || got.NoteURL == "" || got.TaskCount != 0 {
			t.Errorf("got %+v", got)
		}
		n := r.notes.created[0]
		if n.CaptureID != 5 || !n.ReceivedAt.Equal(c.ReceivedAt) || n.Content != "buy milk\n\n#inbox" {
			t.Errorf("note %+v", n)
		}
		a := r.notes.attached
		if len(a) != 1 || a[0].name != "memos/abc" || a[0].filename != "voice-2026-01-02.ogg" ||
			a[0].mime != "audio/ogg" ||
			string(a[0].data) != "AUDIO" {
			t.Errorf("attached %+v", a)
		}
	})
	t.Run("empty transcript shows placeholder", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		cc := c
		cc.Text = ""
		r.run(capture.StepWriteNote, cc)
		if !strings.Contains(r.notes.created[0].Content, "(no speech recognized)") {
			t.Errorf("content %q", r.notes.created[0].Content)
		}
	})
	t.Run("failure becomes placeholder", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		cc := c
		cc.Failure = "http 415"
		r.run(capture.StepWriteNote, cc)
		if !strings.Contains(r.notes.created[0].Content, "http 415") || len(r.notes.attached) != 1 {
			t.Errorf("content %q attached %v", r.notes.created[0].Content, r.notes.attached)
		}
	})
	t.Run("text capture has no attachment", func(t *testing.T) {
		r := newRig(t)
		cc := base()
		cc.Kind, cc.Text = capture.KindText, "note"
		_, out := r.run(capture.StepWriteNote, cc)
		if out.Class != capture.ClassOK || len(r.notes.attached) != 0 {
			t.Errorf("out %+v attached %v", out, r.notes.attached)
		}
	})
	t.Run("minimal note keeps text, drops attachment", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		cc := c
		cc.MinimalNote = true
		_, out := r.run(capture.StepWriteNote, cc)
		content := r.notes.created[0].Content
		if out.Class != capture.ClassOK || len(r.notes.attached) != 0 || content != "buy milk\n\n#inbox" {
			t.Errorf("out %+v content %q attached %v", out, content, r.notes.attached)
		}
	})
	t.Run("minimal note truncates to the limit", func(t *testing.T) {
		r := newRig(t)
		cc := c
		cc.MinimalNote, cc.Text = true, strings.Repeat("x", 5000)
		r.run(capture.StepWriteNote, cc)
		if got := r.notes.created[0].Content; len(got) > 1000 || !strings.Contains(got, "(truncated)") {
			t.Errorf("len %d", len(got))
		}
	})
	t.Run("missing audio file still saves the note", func(t *testing.T) {
		r := newRig(t)
		_, out := r.run(capture.StepWriteNote, c)
		if out.Class != capture.ClassOK || len(r.notes.attached) != 0 {
			t.Errorf("out %+v", out)
		}
	})
}

func TestWriteNoteFailures(t *testing.T) {
	c := base()
	c.AudioPath, c.Text = "5.ogg", "buy milk"
	t.Run("create failure passes through", func(t *testing.T) {
		r := newRig(t)
		r.notes.createOut = capture.Temporary("http 503").On(capture.WaitingMemos)
		got, out := r.run(capture.StepWriteNote, c)
		if out.Class != capture.ClassTemporary || got.NoteName != "" || len(r.notes.attached) != 0 {
			t.Errorf("out %+v got %+v", out, got)
		}
	})
	t.Run("attach failure keeps the note name", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		r.notes.attachOut = capture.Permanent("http 413").On(capture.WaitingMemos)
		got, out := r.run(capture.StepWriteNote, c)
		if out.Class != capture.ClassPermanent || got.NoteName != "memos/abc" {
			t.Errorf("out %+v got %+v", out, got)
		}
	})
}

func TestFinalizeReply(t *testing.T) {
	const link = `<a href="https://memos.example.org/memos/abc?a=1&amp;b=2">open</a>`
	done := func() capture.Capture {
		c := base()
		c.ReplyMessageID, c.NoteName = 9, "memos/abc"
		c.NoteURL = "https://memos.example.org/memos/abc?a=1&b=2"
		return c
	}
	tests := []struct {
		name string
		mod  func(*capture.Capture)
		want string
	}{
		{"done", func(*capture.Capture) {}, "✓ saved · " + link},
		{
			"disabled llm is plain",
			func(c *capture.Capture) { c.LLMSkipped = capture.SkippedDisabled },
			"✓ saved · " + link,
		},
		{
			"unavailable llm",
			func(c *capture.Capture) { c.LLMSkipped = capture.SkippedUnavailable },
			"✓ saved (no AI) · " + link,
		},
		{
			"invalid llm output",
			func(c *capture.Capture) { c.LLMSkipped = capture.SkippedInvalidOutput },
			"✓ saved (no AI) · " + link,
		},
		{"tasks", func(c *capture.Capture) { c.TaskCount = 2 }, "✓ saved · 2 tasks · " + link},
		{"one task", func(c *capture.Capture) { c.TaskCount = 1 }, "✓ saved · 1 task · " + link},
		{
			"failure",
			func(c *capture.Capture) { c.Failure = "http 415 <x>" },
			"⚠ saved audio only — http 415 &lt;x&gt; · " + link,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			c := done()
			tt.mod(&c)
			got, out := r.run(capture.StepFinalizeReply, c)
			if out.Class != capture.ClassOK || len(r.rep.edits) != 1 || len(r.rep.sends) != 0 {
				t.Fatalf("out %+v edits %v sends %v", out, r.rep.edits, r.rep.sends)
			}
			if e := r.rep.edits[0]; e.text != tt.want || e.msg != 9 || e.chat != 10 {
				t.Errorf("edit %+v", e)
			}
			if got.ReplyMessageID != 9 {
				t.Errorf("reply id %d", got.ReplyMessageID)
			}
		})
	}
	t.Run("edit fallback id is recorded", func(t *testing.T) {
		r := newRig(t)
		r.rep.editID = 88
		got, _ := r.run(capture.StepFinalizeReply, done())
		if got.ReplyMessageID != 88 {
			t.Errorf("id %d", got.ReplyMessageID)
		}
	})
	t.Run("no reply yet sends new", func(t *testing.T) {
		r := newRig(t)
		c := done()
		c.ReplyMessageID = 0
		got, out := r.run(capture.StepFinalizeReply, c)
		if out.Class != capture.ClassOK || len(r.rep.sends) != 1 || r.rep.sends[0].replyTo != 20 ||
			got.ReplyMessageID != 77 {
			t.Errorf("out %+v sends %v got %d", out, r.rep.sends, got.ReplyMessageID)
		}
	})
	t.Run("SendNewReply sends new", func(t *testing.T) {
		r := newRig(t)
		c := done()
		c.SendNewReply = true
		_, out := r.run(capture.StepFinalizeReply, c)
		if out.Class != capture.ClassOK || len(r.rep.sends) != 1 || len(r.rep.edits) != 0 {
			t.Errorf("out %+v sends %v edits %v", out, r.rep.sends, r.rep.edits)
		}
	})
	t.Run("edit error is temporary on telegram", func(t *testing.T) {
		r := newRig(t)
		r.rep.editErr = errNet
		_, out := r.run(capture.StepFinalizeReply, done())
		if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingTelegram {
			t.Errorf("out %+v", out)
		}
	})
	t.Run("send error is temporary", func(t *testing.T) {
		r := newRig(t)
		r.rep.sendErr = errNet
		c := done()
		c.ReplyMessageID = 0
		_, out := r.run(capture.StepFinalizeReply, c)
		if out.Class != capture.ClassTemporary {
			t.Errorf("out %+v", out)
		}
	})
}

func TestCleanup(t *testing.T) {
	full := func() capture.Capture {
		c := base()
		c.AudioPath, c.Text, c.Language, c.EnrichedJSON = "5.ogg", "t", "en", "{}"
		c.NoteName, c.NoteURL, c.ReplyMessageID = "memos/abc", "https://memos.example.org/x", 9
		return c
	}
	t.Run("deletes audio and clears content", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		got, out := r.run(capture.StepCleanup, full())
		if out.Class != capture.ClassOK || len(r.audio.files) != 0 {
			t.Fatalf("out %+v files %v", out, r.audio.files)
		}
		if got.Text != "" || got.Language != "" || got.EnrichedJSON != "" || got.AudioPath != "" || got.FileID != "" {
			t.Errorf("not cleared: %+v", got)
		}
		if got.NoteName != "memos/abc" || got.ReplyMessageID != 9 {
			t.Errorf("identity lost: %+v", got)
		}
	})
	t.Run("idempotent without file", func(t *testing.T) {
		r := newRig(t)
		_, out := r.run(capture.StepCleanup, full())
		if out.Class != capture.ClassOK {
			t.Errorf("out %+v", out)
		}
	})
	t.Run("pending new reply is delivered before clearing", func(t *testing.T) {
		r := newRig(t)
		c := full()
		c.SendNewReply = true
		got, out := r.run(capture.StepCleanup, c)
		if out.Class != capture.ClassOK || len(r.rep.sends) != 1 || !strings.HasPrefix(r.rep.sends[0].text, "✓ saved") {
			t.Fatalf("out %+v sends %v", out, r.rep.sends)
		}
		if got.ReplyMessageID != 77 || got.SendNewReply {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("pending reply failure keeps content for retry", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		r.rep.sendErr = errNet
		c := full()
		c.SendNewReply = true
		got, out := r.run(capture.StepCleanup, c)
		if out.Class != capture.ClassTemporary || got.NoteURL == "" || !got.SendNewReply {
			t.Errorf("out %+v got %+v", out, got)
		}
	})
}

func TestRejectedReplyIsDone(t *testing.T) {
	rejected := fmt.Errorf("telegram: %w", pipeline.ErrRejected)
	t.Run("finalize edit", func(t *testing.T) {
		r := newRig(t)
		r.rep.editErr = rejected
		c := base()
		c.ReplyMessageID, c.NoteURL = 9, "https://memos.example.org/x"
		got, out := r.run(capture.StepFinalizeReply, c)
		if out.Class != capture.ClassOK || got.ReplyMessageID != 9 {
			t.Errorf("out %+v got %+v", out, got)
		}
	})
	t.Run("finalize send", func(t *testing.T) {
		r := newRig(t)
		r.rep.sendErr = rejected
		c := base()
		c.NoteURL = "https://memos.example.org/x"
		if _, out := r.run(capture.StepFinalizeReply, c); out.Class != capture.ClassOK {
			t.Errorf("out %+v", out)
		}
	})
	t.Run("cleanup pending send still cleans up", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		r.rep.sendErr = rejected
		c := base()
		c.AudioPath, c.Text, c.SendNewReply = "5.ogg", "t", true
		got, out := r.run(capture.StepCleanup, c)
		if out.Class != capture.ClassOK || len(r.audio.files) != 0 || got.Text != "" || got.SendNewReply {
			t.Errorf("out %+v got %+v files %v", out, got, r.audio.files)
		}
	})
}

func TestAttachFailureKeepsTextAndSaysSo(t *testing.T) {
	c := base()
	c.AudioPath, c.Text = "5.ogg", "buy milk"
	t.Run("permanent attach records the reason", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		r.notes.attachOut = capture.Permanent("http 413").On(capture.WaitingMemos)
		got, out := r.run(capture.StepWriteNote, c)
		if out.Class != capture.ClassPermanent || got.LastError != "http 413" {
			t.Errorf("out %+v got %+v", out, got)
		}
	})
	t.Run("minimal rerun keeps text and marks the failure", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		cc := c
		cc.MinimalNote, cc.NoteName, cc.LastError = true, "memos/abc", "http 413"
		got, out := r.run(capture.StepWriteNote, cc)
		if out.Class != capture.ClassOK || len(r.notes.attached) != 0 ||
			r.notes.created[0].Content != "buy milk\n\n#inbox" || got.Failure == "" {
			t.Errorf("out %+v got %+v content %q", out, got, r.notes.created[0].Content)
		}
	})
	t.Run("minimal note without a recorded reason still says so", func(t *testing.T) {
		r := newRig(t)
		cc := c
		cc.MinimalNote = true
		got, _ := r.run(capture.StepWriteNote, cc)
		if got.Failure == "" {
			t.Error("want the failure recorded")
		}
	})
	t.Run("reply names the failure", func(t *testing.T) {
		r := newRig(t)
		r.audio.files["5.ogg"] = []byte("A")
		cc := c
		cc.MinimalNote, cc.NoteName, cc.LastError = true, "memos/abc", "http <413>"
		got, _ := r.run(capture.StepWriteNote, cc)
		got.ReplyMessageID, got.NoteURL = 9, "https://memos.example.org/x"
		_, out := r.run(capture.StepFinalizeReply, got)
		want := `⚠ saved, audio not attached — http &lt;413&gt; · <a href="https://memos.example.org/x">open</a>`
		if out.Class != capture.ClassOK || r.rep.edits[0].text != want {
			t.Errorf("edit %q", r.rep.edits[0].text)
		}
	})
}

func TestTranscribeSendsTheStoredExtension(t *testing.T) {
	for _, tt := range []struct{ path, want string }{{"5.mp3", ".mp3"}, {"5.ogg", ".ogg"}} {
		r := newRig(t)
		r.audio.files = map[string][]byte{tt.path: []byte("A")}
		c := base()
		c.AudioPath = tt.path
		if _, out := r.run(capture.StepTranscribe, c); out.Class != capture.ClassOK || r.tr.req.Ext != tt.want {
			t.Errorf("%s: out %+v ext %q, want %q", tt.path, out, r.tr.req.Ext, tt.want)
		}
	}
}

func TestAttachNameIsDeterministicAndPrivate(t *testing.T) {
	// 23:30 UTC on Jan 2 is already Jan 3 in Tokyo.
	recv := time.Date(2026, 1, 2, 23, 30, 0, 0, time.UTC)
	tokyo := time.FixedZone("JST", 9*3600)
	tests := []struct {
		name string
		loc  *time.Location
		path string
		want string
	}{
		{"default UTC", nil, "tg-10-20-123.ogg", "voice-2026-01-02.ogg"},
		{"configured zone", tokyo, "tg-10-20-123.ogg", "voice-2026-01-03.ogg"},
		{"extension kept", nil, "5.mp3", "voice-2026-01-02.mp3"},
		{"no extension defaults to ogg", nil, "5", "voice-2026-01-02.ogg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRigWith(t, pipeline.StepsConfig{
				InboxTag: "inbox", TodoTag: "todo", NoteLimit: 1000, Location: tt.loc,
			})
			r.audio.files[tt.path] = []byte("A")
			c := base()
			c.ReceivedAt, c.AudioPath, c.Text = recv, tt.path, "t"
			if _, out := r.run(capture.StepWriteNote, c); out.Class != capture.ClassOK {
				t.Fatalf("out %+v", out)
			}
			if len(r.notes.attached) != 1 || r.notes.attached[0].filename != tt.want {
				t.Errorf("attached %+v, want filename %q", r.notes.attached, tt.want)
			}
		})
	}
}
