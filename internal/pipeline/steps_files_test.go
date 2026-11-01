package pipeline_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/enrich"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/render"
)

var errTooLarge = fmt.Errorf("telegram: %w", pipeline.ErrFileTooLarge)

// TestRenderConstantsMatchCapture pins render's plain string kinds and states
// to the capture values the pipeline converts from.
func TestRenderConstantsMatchCapture(t *testing.T) {
	pairs := []struct{ render, capture string }{
		{render.KindImage, capture.FileImage.String()},
		{render.KindDocument, capture.FileDocument.String()},
		{render.StateDone, capture.FileDone.String()},
		{render.StateEmpty, capture.FileEmpty.String()},
		{render.StateFailed, capture.FileFailed.String()},
	}
	for _, p := range pairs {
		if p.render != p.capture {
			t.Errorf("render %q != capture %q", p.render, p.capture)
		}
	}
}

// filesCapture is a files capture with the given files at positions 0..n-1.
func filesCapture(files ...capture.File) capture.Capture {
	c := base()
	c.Kind = capture.KindFiles
	for i := range files {
		files[i].Position = i
	}
	c.Files = files

	return c
}

func TestCollectReturnsOK(t *testing.T) {
	r := newRig(t)
	c := filesCapture(capture.File{Kind: capture.FileImage, State: capture.FilePending, FileID: "P"})
	h := r.h[capture.StepCollect]
	if h == nil {
		t.Fatal("no collect handler")
	}
	got, out := h(context.Background(), c)
	if out.Class != capture.ClassOK || !reflect.DeepEqual(got, c) {
		t.Errorf("out %+v capture %+v", out, got)
	}
}

func TestExtractSkipsUnreadFiles(t *testing.T) {
	r := newRig(t)
	c := filesCapture(
		capture.File{Kind: capture.FileImage, State: capture.FilePending, LocalPath: "5.jpg"},
		capture.File{Kind: capture.FileDocument, State: capture.FilePending, LocalPath: "5-1.pdf"},
		capture.File{Kind: capture.FileVideo, State: capture.FilePending, LocalPath: "5-2.mp4"},
		capture.File{Kind: capture.FileDocument, State: capture.FileFailed, Failure: "too large for Telegram bots"},
	)
	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassOK {
		t.Fatalf("out %+v", out)
	}
	want := []capture.FileState{capture.FileSkipped, capture.FileSkipped, capture.FileSkipped, capture.FileFailed}
	for i, f := range got.Files {
		if f.State != want[i] {
			t.Errorf("file %d state %q, want %q", i, f.State, want[i])
		}
	}
	if got.Files[3].Failure != "too large for Telegram bots" {
		t.Errorf("failed file changed: %+v", got.Files[3])
	}
	if r.tr.call != 0 {
		t.Errorf("transcriber called %d times", r.tr.call)
	}
	if c.Files[0].State != capture.FilePending {
		t.Error("the handler must not write through to the caller's files")
	}
	if got.Text != c.Text {
		t.Errorf("caption changed to %q", got.Text)
	}
}

func TestEnrichFilesCaption(t *testing.T) {
	opts := enrich.Options{Translate: true, Tidy: true, Tasks: true, Target: "en", MaxTags: 3}
	t.Run("caption only runs the existing request on the caption", func(t *testing.T) {
		e := &fakeEnricher{raw: `{"language":"en","text":"Pay the bill.","tasks":["Pay the bill"]}`, out: capture.OK()}
		c := filesCapture(capture.File{Kind: capture.FileImage, State: capture.FileSkipped, LocalPath: "5.jpg"})
		c.Text = "pay the bill"
		got, out := llmHandlers(t, e)[capture.StepEnrich](context.Background(), c)
		if out.Class != capture.ClassOK || e.calls != 1 {
			t.Fatalf("out %+v calls %d", out, e.calls)
		}
		if want := enrich.BuildRequest(opts, "pay the bill", ""); !reflect.DeepEqual(e.got, want) {
			t.Errorf("request %+v, want %+v", e.got, want)
		}
		if got.EnrichedJSON == "" || got.TaskCount != 1 {
			t.Errorf("stored %q tasks %d", got.EnrichedJSON, got.TaskCount)
		}
	})
	t.Run("no caption records no call", func(t *testing.T) {
		e := &fakeEnricher{raw: `{"text":"x"}`, out: capture.OK()}
		c := filesCapture(capture.File{Kind: capture.FileImage, State: capture.FileSkipped, LocalPath: "5.jpg"})
		got, out := llmHandlers(t, e)[capture.StepEnrich](context.Background(), c)
		if out.Class != capture.ClassOK || e.calls != 0 || got.EnrichedJSON != "" || got.LLMSkipped != "" {
			t.Errorf("out %+v calls %d stored %q skipped %q", out, e.calls, got.EnrichedJSON, got.LLMSkipped)
		}
	})
}

// writeAll runs write_note until it stops returning progress.
func writeAll(t *testing.T, r *rig, c capture.Capture) (capture.Capture, capture.Outcome) {
	t.Helper()
	for range 10 {
		var out capture.Outcome
		c, out = r.run(capture.StepWriteNote, c)
		if out.Class != capture.ClassProgress {
			return c, out
		}
	}
	t.Fatal("write_note never finished")

	return c, capture.Outcome{}
}

func TestWriteFilesNote(t *testing.T) {
	r := newRig(t)
	r.audio.files["5.jpg"] = []byte("JPG")
	r.audio.files["5-1.pdf"] = []byte("PDF")
	r.audio.files["5-2.bin"] = []byte("BIN")
	c := filesCapture(
		capture.File{Kind: capture.FileImage, State: capture.FileSkipped, MimeType: "image/jpeg", LocalPath: "5.jpg"},
		capture.File{
			Kind: capture.FileDocument, State: capture.FileSkipped, MimeType: "application/pdf",
			FileName: "bill.pdf", LocalPath: "5-1.pdf",
		},
		capture.File{
			Kind:      capture.FileDocument,
			State:     capture.FileSkipped,
			FileName:  "data.bin",
			LocalPath: "5-2.bin",
		},
	)
	c.Text = "March bills"

	got, out := writeAll(t, r, c)
	if out.Class != capture.ClassOK {
		t.Fatalf("out %+v", out)
	}
	if len(r.notes.created) != 1 {
		t.Fatalf("creates %d", len(r.notes.created))
	}
	if want := "March bills\n\n📎 3 files\n\n#inbox"; r.notes.created[0].Content != want {
		t.Errorf("content %q, want %q", r.notes.created[0].Content, want)
	}
	want := []attachCall{
		{"memos/abc", "photo-2026-01-02-1.jpg", "image/jpeg", []byte("JPG")},
		{"memos/abc", "bill.pdf", "application/pdf", []byte("PDF")},
		{"memos/abc", "data.bin", "application/octet-stream", []byte("BIN")},
	}
	if !reflect.DeepEqual(r.notes.attached, want) {
		t.Errorf("attached %+v, want %+v", r.notes.attached, want)
	}
	for i, f := range got.Files {
		if !f.Attached || f.AttachFailure != "" {
			t.Errorf("file %d %+v", i, f)
		}
	}
	if got.NoteName != "memos/abc" || got.MinimalNote || got.Failure != "" {
		t.Errorf("capture %+v", got)
	}
}

func TestWriteFilesNoteHint(t *testing.T) {
	tests := []struct {
		name  string
		files []capture.File
		text  string
		want  string
	}{
		{
			name:  "one photo, no caption",
			files: []capture.File{{Kind: capture.FileImage, State: capture.FileSkipped, LocalPath: "5.jpg"}},
			want:  "📷 photo\n\n#inbox",
		},
		{
			name: "one named document with caption",
			files: []capture.File{
				{Kind: capture.FileDocument, State: capture.FileSkipped, FileName: "report.pdf", LocalPath: "5.pdf"},
			},
			text: "Q3 report",
			want: "Q3 report\n\n📎 report.pdf\n\n#inbox",
		},
		{
			name: "nothing kept names the reason",
			files: []capture.File{
				{Kind: capture.FileImage, State: capture.FileFailed, Failure: "too large for Telegram bots"},
				{Kind: capture.FileImage, State: capture.FileFailed, Failure: "unavailable from Telegram"},
			},
			want: "📷 2 photos — too large for Telegram bots\n\n#inbox",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.audio.files["5.jpg"], r.audio.files["5.pdf"] = []byte("J"), []byte("P")
			c := filesCapture(tc.files...)
			c.Text = tc.text
			if _, out := writeAll(t, r, c); out.Class != capture.ClassOK {
				t.Fatalf("out %+v", out)
			}
			if got := r.notes.created[0].Content; got != tc.want {
				t.Errorf("content %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteFilesNoteUsesTheEnrichedCaption(t *testing.T) {
	r := newRig(t)
	r.audio.files["5.jpg"] = []byte("J")
	c := filesCapture(capture.File{Kind: capture.FileImage, State: capture.FileSkipped, LocalPath: "5.jpg"})
	c.Text = "оплатить счёт"
	c.EnrichedJSON = `{"language":"ru","text":"Pay the bill","translated":true,"tasks":["Pay the bill"],"tags":["home"]}`
	got, out := writeAll(t, r, c)
	if out.Class != capture.ClassOK {
		t.Fatalf("out %+v", out)
	}
	want := "Pay the bill\n\n📷 photo\n\n- [ ] Pay the bill\n\n*Original (ru):*\n> оплатить счёт\n\n#inbox #todo #home"
	if content := r.notes.created[0].Content; content != want {
		t.Errorf("content %q, want %q", content, want)
	}
	if got.TaskCount != 1 {
		t.Errorf("tasks %d", got.TaskCount)
	}
}

func TestWriteFilesNoteAttachFailure(t *testing.T) {
	held := func(r *rig) capture.Capture {
		r.audio.files["5.pdf"], r.audio.files["5-1.pdf"] = []byte("A"), []byte("B")

		return filesCapture(
			capture.File{Kind: capture.FileDocument, State: capture.FileSkipped, FileName: "a.pdf", LocalPath: "5.pdf"},
			capture.File{
				Kind:      capture.FileDocument,
				State:     capture.FileSkipped,
				FileName:  "b.pdf",
				LocalPath: "5-1.pdf",
			},
		)
	}
	t.Run("permanent is recorded and the note stands", func(t *testing.T) {
		r := newRig(t)
		r.notes.attachOut = capture.Permanent("http 413")
		got, out := r.run(capture.StepWriteNote, held(r))
		if out.Class != capture.ClassProgress {
			t.Fatalf("out %+v, want progress", out)
		}
		if f := got.Files[0]; !f.Attached || f.AttachFailure != "http 413" || f.LocalPath == "" {
			t.Errorf("file %+v", f)
		}
		r.notes.attachOut = capture.OK()
		got, out = r.run(capture.StepWriteNote, got)
		if out.Class != capture.ClassOK || len(r.notes.attached) != 2 || !got.Files[1].Attached ||
			got.Files[1].AttachFailure != "" || len(r.notes.created) != 1 || got.MinimalNote {
			t.Errorf("out %+v attached %d files %+v", out, len(r.notes.attached), got.Files)
		}
	})
	t.Run("temporary is retried", func(t *testing.T) {
		r := newRig(t)
		r.notes.attachOut = capture.Temporary("http 503")
		got, out := r.run(capture.StepWriteNote, held(r))
		if out.Class != capture.ClassTemporary {
			t.Fatalf("out %+v", out)
		}
		if f := got.Files[0]; f.Attached || f.AttachFailure != "" {
			t.Errorf("file %+v", f)
		}
		if got.NoteName != "memos/abc" {
			t.Errorf("note name %q", got.NoteName)
		}
	})
	t.Run("a minimal-note retry keeps the attachments", func(t *testing.T) {
		r := newRig(t)
		c := held(r)
		c.MinimalNote = true
		c.EnrichedJSON = `{"text":"Tidy","tasks":["x"]}`
		c.Text = "raw caption"
		got, out := writeAll(t, r, c)
		if out.Class != capture.ClassOK || len(r.notes.attached) != 2 || got.Failure != "" {
			t.Fatalf("out %+v attached %d failure %q", out, len(r.notes.attached), got.Failure)
		}
		if content := r.notes.created[0].Content; !strings.HasPrefix(content, "raw caption\n\n") {
			t.Errorf("content %q", content)
		}
	})
}

func TestFilesReply(t *testing.T) {
	const link = `<a href="https://memos.example.org/x">open</a>`
	kept := func() capture.File {
		return capture.File{Kind: capture.FileImage, State: capture.FileSkipped, LocalPath: "5.jpg", Attached: true}
	}
	tooLarge := capture.File{Kind: capture.FileImage, State: capture.FileFailed, Failure: "too large for Telegram bots"}
	refused := kept()
	refused.AttachFailure = "http 413 <big>"
	tests := []struct {
		name    string
		files   []capture.File
		tasks   int
		skipped string
		want    string
	}{
		{name: "all kept", files: []capture.File{kept(), kept()}, want: "✓ saved · " + link},
		{
			name: "tasks and no AI", files: []capture.File{kept()}, tasks: 2, skipped: capture.SkippedUnavailable,
			want: "✓ saved (no AI) · 2 tasks · " + link,
		},
		{
			name:  "some not kept",
			files: []capture.File{kept(), tooLarge, kept(), kept()},
			want:  "⚠ saved, 1 of 4 not kept — too large for Telegram bots · " + link,
		},
		{
			name:  "some not attached",
			files: []capture.File{kept(), refused},
			want:  "⚠ saved, 1 of 2 not attached — http 413 &lt;big&gt; · " + link,
		},
		{
			name:  "not kept wins over not attached",
			files: []capture.File{refused, tooLarge, kept()},
			want:  "⚠ saved, 1 of 3 not kept — too large for Telegram bots · " + link,
		},
		{
			name:  "a file missing at attach is not kept",
			files: []capture.File{kept(), {Kind: capture.FileDocument, State: capture.FileSkipped}},
			want:  "⚠ saved, 1 of 2 not kept — file missing · " + link,
		},
		{
			name:  "nothing kept",
			files: []capture.File{tooLarge, tooLarge},
			want:  "⚠ files not kept — too large for Telegram bots · please send them again · " + link,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			c := filesCapture(tc.files...)
			c.ReplyMessageID, c.NoteURL = 9, "https://memos.example.org/x"
			c.TaskCount, c.LLMSkipped = tc.tasks, tc.skipped
			if _, out := r.run(capture.StepFinalizeReply, c); out.Class != capture.ClassOK {
				t.Fatalf("out %+v", out)
			}
			if len(r.rep.edits) != 1 || r.rep.edits[0].text != tc.want {
				t.Errorf("edits %+v, want %q", r.rep.edits, tc.want)
			}
		})
	}
}

// TestFilesCaptureEndToEnd drives a two-photo capture from collect to done
// with the real policy: one photo is too large, the other is attached.
func TestFilesCaptureEndToEnd(t *testing.T) {
	r := newRig(t)
	c := filesCapture(
		capture.File{Kind: capture.FileImage, State: capture.FilePending, FileID: "A", MimeType: "image/jpeg"},
		capture.File{Kind: capture.FileImage, State: capture.FilePending, FileID: "B", MimeType: "image/jpeg"},
	)
	c.Step, c.Text = capture.StepCollect, "receipts"
	pol := capture.Policy{MaxInterval: time.Minute}
	now := c.ReceivedAt
	for i := 0; i < 30 && c.Step != capture.StepDone; i++ {
		if c.Step == capture.StepDownload && c.Files[0].LocalPath != "" {
			r.dl.err = errTooLarge
		}
		got, out := r.run(c.Step, c)
		c = pol.Next(got, out, now)
		now = now.Add(time.Hour)
	}
	if c.Step != capture.StepDone {
		t.Fatalf("stuck at %s", c.Step)
	}
	if want := "receipts\n\n📷 2 photos\n\n#inbox"; len(r.notes.created) != 1 || r.notes.created[0].Content != want {
		t.Errorf("created %+v", r.notes.created)
	}
	if len(r.notes.attached) != 1 || r.notes.attached[0].filename != "photo-2026-01-02-1.jpg" ||
		r.notes.attached[0].mime != "image/jpeg" {
		t.Errorf("attached %+v", r.notes.attached)
	}
	wantReply := `⚠ saved, 1 of 2 not kept — too large for Telegram bots · ` +
		`<a href="https://memos.example.org/memos/abc?a=1&amp;b=2">open</a>`
	if len(r.rep.edits) != 1 || r.rep.edits[0].text != wantReply {
		t.Errorf("sends %+v edits %+v", r.rep.sends, r.rep.edits)
	}
}
