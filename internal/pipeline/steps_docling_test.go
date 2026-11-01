package pipeline_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/enrich"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// extractCall records one Convert call.
type extractCall struct {
	name, mime string
	data       []byte
}

// extractAnswer is one scripted Convert answer.
type extractAnswer struct {
	res pipeline.ExtractResult
	out capture.Outcome
}

// fakeExtractor answers Convert from a script, repeating the last answer.
type fakeExtractor struct {
	answers []extractAnswer
	calls   []extractCall
}

func (f *fakeExtractor) Convert(
	_ context.Context, name, mime string, data []byte,
) (pipeline.ExtractResult, capture.Outcome) {
	f.calls = append(f.calls, extractCall{name, mime, data})
	a := f.answers[min(len(f.calls), len(f.answers))-1]

	return a.res, a.out
}

func readOK(text string) extractAnswer {
	return extractAnswer{res: pipeline.ExtractResult{Markdown: text}, out: capture.OK()}
}

// doclingRig is a rig whose handlers read documents through e.
func doclingRig(t *testing.T, e *fakeExtractor) *rig {
	t.Helper()
	r := newRig(t)
	h, err := pipeline.NewHandlers(pipeline.Deps{
		Transcriber: r.tr, Notes: r.notes, Replier: r.rep, Downloader: r.dl, Files: r.audio, Extractor: e,
	}, pipeline.StepsConfig{InboxTag: "inbox", TodoTag: "todo", NoteLimit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	r.h = h

	return r
}

// docsCapture is a files capture with a photo, a PDF and a video, all held.
func docsCapture(r *rig) capture.Capture {
	r.audio.files["5.jpg"] = []byte("JPG")
	r.audio.files["5-1.pdf"] = []byte("PDF")
	r.audio.files["5-2.mp4"] = []byte("MP4")

	return filesCapture(
		capture.File{Kind: capture.FileImage, State: capture.FilePending, MimeType: "image/jpeg", LocalPath: "5.jpg"},
		capture.File{
			Kind: capture.FileDocument, State: capture.FilePending, MimeType: "application/pdf",
			FileName: "dir/bill.pdf", LocalPath: "5-1.pdf",
		},
		capture.File{Kind: capture.FileVideo, State: capture.FilePending, MimeType: "video/mp4", LocalPath: "5-2.mp4"},
	)
}

func TestExtractReadsDocumentsOnePerAttempt(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{readOK("# Receipt\nTotal 12"), readOK(" \n\t")}}
	r := doclingRig(t, e)
	c := docsCapture(r)

	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassProgress || len(e.calls) != 1 {
		t.Fatalf("first attempt: out %+v calls %d", out, len(e.calls))
	}
	// The nameless photo goes under its attachment name, extension included.
	want := extractCall{"photo-2026-01-02-1.jpg", "image/jpeg", []byte("JPG")}
	if !reflect.DeepEqual(e.calls[0], want) {
		t.Errorf("call %+v, want %+v", e.calls[0], want)
	}
	f := got.Files[0]
	if f.State != capture.FileDone || f.Text != "# Receipt\nTotal 12" || f.TextSource != capture.TextDocling {
		t.Errorf("photo %+v", f)
	}
	if got.Files[1].State != capture.FilePending || got.Files[2].State != capture.FileSkipped {
		t.Errorf("rest %+v", got.Files[1:])
	}
	if c.Files[0].State != capture.FilePending {
		t.Error("the handler must not write through to the caller's files")
	}

	got, out = r.run(capture.StepExtract, got)
	if out.Class != capture.ClassOK || len(e.calls) != 2 {
		t.Fatalf("second attempt: out %+v calls %d", out, len(e.calls))
	}
	if want := (extractCall{"bill.pdf", "application/pdf", []byte("PDF")}); !reflect.DeepEqual(e.calls[1], want) {
		t.Errorf("call %+v, want %+v", e.calls[1], want)
	}
	f = got.Files[1]
	if f.State != capture.FileEmpty || f.Text != "" || f.TextSource != capture.TextDocling {
		t.Errorf("pdf %+v", f)
	}
	if got.Files[0].State != capture.FileDone {
		t.Errorf("photo changed: %+v", got.Files[0])
	}

	// A re-run after the last file sends nothing again.
	if _, out = r.run(capture.StepExtract, got); out.Class != capture.ClassOK || len(e.calls) != 2 {
		t.Errorf("re-run: out %+v calls %d", out, len(e.calls))
	}
}

func TestExtractDoclingPermanentFailsTheFileOnly(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{
		{out: capture.Permanent("document type not supported").On(capture.WaitingDocling)},
		readOK("text"),
	}}
	r := doclingRig(t, e)
	got, out := r.run(capture.StepExtract, docsCapture(r))
	if out.Class != capture.ClassProgress {
		t.Fatalf("out %+v", out)
	}
	f := got.Files[0]
	if f.State != capture.FileFailed || f.Failure != "document type not supported" || f.Text != "" ||
		f.LocalPath != "5.jpg" {
		t.Errorf("photo %+v", f)
	}
	got, out = r.run(capture.StepExtract, got)
	if out.Class != capture.ClassOK || got.Files[1].State != capture.FileDone || len(e.calls) != 2 {
		t.Errorf("out %+v pdf %+v calls %d", out, got.Files[1], len(e.calls))
	}
}

func TestExtractDoclingPermanentOnLastFileIsOK(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{{out: capture.Permanent("document reader could not read it")}}}
	r := doclingRig(t, e)
	r.audio.files["5.pdf"] = []byte("PDF")
	c := filesCapture(capture.File{Kind: capture.FileDocument, State: capture.FilePending, LocalPath: "5.pdf"})
	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassOK || got.Files[0].State != capture.FileFailed ||
		got.Files[0].Failure != "document reader could not read it" {
		t.Errorf("out %+v file %+v", out, got.Files[0])
	}
}

func TestExtractDoclingTimeout(t *testing.T) {
	timedOut := extractAnswer{
		res: pipeline.ExtractResult{TimedOut: true},
		out: capture.Temporary("timed out").On(capture.WaitingDocling),
	}
	t.Run("an early timeout waits on docling and keeps the file pending", func(t *testing.T) {
		e := &fakeExtractor{answers: []extractAnswer{timedOut}}
		r := doclingRig(t, e)
		got, out := r.run(capture.StepExtract, docsCapture(r))
		if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingDocling {
			t.Fatalf("out %+v", out)
		}
		if f := got.Files[0]; f.State != capture.FilePending || f.Timeouts != 1 || f.Failure != "" {
			t.Errorf("photo %+v", f)
		}
	})
	t.Run("the third timeout fails the file and goes on", func(t *testing.T) {
		e := &fakeExtractor{answers: []extractAnswer{timedOut}}
		r := doclingRig(t, e)
		c := docsCapture(r)
		c.Files[0].Timeouts = 2
		got, out := r.run(capture.StepExtract, c)
		if out.Class != capture.ClassProgress {
			t.Fatalf("out %+v", out)
		}
		if f := got.Files[0]; f.State != capture.FileFailed || f.Timeouts != 3 || f.Failure != "couldn't read in time" {
			t.Errorf("photo %+v", f)
		}
	})
}

func TestExtractDoclingUnavailableWaits(t *testing.T) {
	cases := []struct {
		name string
		out  capture.Outcome
		want capture.Class
	}{
		{"unreachable", capture.Temporary("connection refused").On(capture.WaitingDocling), capture.ClassTemporary},
		{"temporary without a dependency", capture.Temporary("http 503"), capture.ClassTemporary},
		{"misconfigured", capture.Misconfig("http 401").On(capture.WaitingDocling), capture.ClassMisconfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &fakeExtractor{answers: []extractAnswer{{out: tc.out}}}
			r := doclingRig(t, e)
			got, out := r.run(capture.StepExtract, docsCapture(r))
			if out.Class != tc.want || out.WaitingOn != capture.WaitingDocling || out.Reason != tc.out.Reason {
				t.Errorf("out %+v", out)
			}
			if f := got.Files[0]; f.State != capture.FilePending || f.Timeouts != 0 || f.Failure != "" {
				t.Errorf("photo %+v", f)
			}
		})
	}
}

func TestExtractDoclingMissingFile(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{readOK("text")}}
	r := doclingRig(t, e)
	c := docsCapture(r)
	delete(r.audio.files, "5.jpg")
	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassProgress || len(e.calls) != 0 {
		t.Fatalf("out %+v calls %d", out, len(e.calls))
	}
	if f := got.Files[0]; f.State != capture.FileFailed || f.LocalPath != "" || f.Failure != "file missing" {
		t.Errorf("photo %+v", f)
	}
}

func TestExtractDoclingKeepsAudioTranscribed(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{readOK("doc")}}
	r := doclingRig(t, e)
	r.tr.res = pipeline.TranscribeResult{Text: "hello"}
	r.audio.files["5.ogg"] = []byte("OGG")
	r.audio.files["5-1.pdf"] = []byte("PDF")
	c := filesCapture(
		capture.File{Kind: capture.FileAudio, State: capture.FilePending, LocalPath: "5.ogg"},
		capture.File{Kind: capture.FileDocument, State: capture.FilePending, LocalPath: "5-1.pdf"},
	)
	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassProgress || r.tr.call != 1 || len(e.calls) != 0 {
		t.Fatalf("out %+v transcribe %d convert %d", out, r.tr.call, len(e.calls))
	}
	got, out = r.run(capture.StepExtract, got)
	if out.Class != capture.ClassOK || len(e.calls) != 1 || got.Files[1].State != capture.FileDone {
		t.Errorf("out %+v convert %d pdf %+v", out, len(e.calls), got.Files[1])
	}
}

// A standard docling-serve (v1.38.2) has no plain-text input format, so
// .txt / text/plain files are read locally and never submitted (FR-110).
func TestExtractReadsPlainTextLocally(t *testing.T) {
	cases := []struct {
		name string
		file capture.File
	}{
		{"mime", capture.File{MimeType: "text/plain", FileName: "notes"}},
		{"mime with parameters", capture.File{MimeType: "Text/Plain; charset=utf-8", FileName: "notes"}},
		{"extension", capture.File{MimeType: "application/octet-stream", FileName: "dir/Notes.TXT"}},
	}
	for _, tc := range cases {
		for _, withReader := range []bool{true, false} {
			t.Run(tc.name, func(t *testing.T) {
				e := &fakeExtractor{answers: []extractAnswer{readOK("from docling")}}
				r := newRig(t)
				if withReader {
					r = doclingRig(t, e)
				}
				r.audio.files["5.txt"] = []byte("héllo\xff wörld")
				f := tc.file
				f.Kind, f.State, f.LocalPath = capture.FileDocument, capture.FilePending, "5.txt"

				got, out := r.run(capture.StepExtract, filesCapture(f))
				if out.Class != capture.ClassOK || len(e.calls) != 0 {
					t.Fatalf("out %+v, docling calls %d", out, len(e.calls))
				}
				g := got.Files[0]
				if g.State != capture.FileDone || g.Text != "héllo� wörld" || g.TextSource != capture.TextLocal {
					t.Errorf("file %+v", g)
				}
			})
		}
	}
}

func TestExtractPlainTextIsBoundedByTheExcerptBudget(t *testing.T) {
	r := newRigWith(t, pipeline.StepsConfig{
		InboxTag: "inbox", TodoTag: "todo", NoteLimit: 1000, Enrich: enrich.Options{ExcerptRunes: 5},
	})
	r.audio.files["5.txt"] = []byte("абвгдежзи")
	c := filesCapture(capture.File{
		Kind: capture.FileDocument, State: capture.FilePending, MimeType: "text/plain", LocalPath: "5.txt",
	})

	got, _ := r.run(capture.StepExtract, c)
	if got.Files[0].Text != "абвгд" {
		t.Errorf("text %q", got.Files[0].Text)
	}
}

func TestExtractEmptyPlainTextIsEmpty(t *testing.T) {
	r := newRig(t)
	r.audio.files["5.txt"] = []byte(" \n")
	c := filesCapture(capture.File{
		Kind: capture.FileDocument, State: capture.FilePending, MimeType: "text/plain", LocalPath: "5.txt",
	})

	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassOK || got.Files[0].State != capture.FileEmpty || got.Files[0].Text != "" {
		t.Errorf("out %+v file %+v", out, got.Files[0])
	}
}

func TestExtractPlainTextMissingFileFails(t *testing.T) {
	r := newRig(t)
	c := filesCapture(capture.File{
		Kind: capture.FileDocument, State: capture.FilePending, MimeType: "text/plain", LocalPath: "gone.txt",
	})

	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassOK || got.Files[0].State != capture.FileFailed {
		t.Errorf("out %+v file %+v", out, got.Files[0])
	}
}

func TestExtractImageMarkersOnlyIsEmpty(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{
		readOK("<!-- image -->\n\n<!-- image -->\n"),
		readOK("<!-- image -->\n# Receipt\nTotal 12"),
	}}
	r := doclingRig(t, e)
	r.audio.files["5.jpg"] = []byte("JPG")
	r.audio.files["5-1.jpg"] = []byte("JPG")
	c := filesCapture(
		capture.File{Kind: capture.FileImage, State: capture.FilePending, MimeType: "image/jpeg", LocalPath: "5.jpg"},
		capture.File{Kind: capture.FileImage, State: capture.FilePending, MimeType: "image/jpeg", LocalPath: "5-1.jpg"},
	)

	got, _ := r.run(capture.StepExtract, c)
	got, _ = r.run(capture.StepExtract, got)
	if f := got.Files[0]; f.State != capture.FileEmpty || f.Text != "" {
		t.Errorf("marker-only photo %+v", f)
	}
	if f := got.Files[1]; f.State != capture.FileDone || f.Text != "# Receipt\nTotal 12" {
		t.Errorf("photo with text %+v", f)
	}
}
