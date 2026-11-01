package pipeline_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// describeCall records one Describe call.
type describeCall struct {
	image []byte
	mime  string
}

// describeAnswer is one scripted Describe answer.
type describeAnswer struct {
	text string
	out  capture.Outcome
}

// fakeDescriber answers Describe from a script, repeating the last answer.
type fakeDescriber struct {
	answers []describeAnswer
	calls   []describeCall
}

func (f *fakeDescriber) Describe(_ context.Context, image []byte, mime string) (string, capture.Outcome) {
	f.calls = append(f.calls, describeCall{image, mime})
	a := f.answers[min(len(f.calls), len(f.answers))-1]

	return a.text, a.out
}

func describeOK(text string) describeAnswer { return describeAnswer{text: text, out: capture.OK()} }

// visionRig is a rig whose handlers describe photos through v and, when e is
// not nil, read documents through e first.
func visionRig(t *testing.T, e *fakeExtractor, v *fakeDescriber) *rig {
	t.Helper()
	r := newRig(t)
	d := pipeline.Deps{
		Transcriber: r.tr, Notes: r.notes, Replier: r.rep, Downloader: r.dl, Files: r.audio, Describer: v,
	}
	if e != nil {
		d.Extractor = e
	}
	h, err := pipeline.NewHandlers(d, pipeline.StepsConfig{InboxTag: "inbox", TodoTag: "todo", NoteLimit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	r.h = h

	return r
}

// runExtract runs extract until it stops returning Progress, failing on any
// outcome other than Progress and OK, or after limit attempts.
func runExtract(t *testing.T, r *rig, c capture.Capture, limit int) capture.Capture {
	t.Helper()
	for range limit {
		var out capture.Outcome
		c, out = r.run(capture.StepExtract, c)
		if out.Class == capture.ClassOK {
			return c
		}
		if out.Class != capture.ClassProgress || out.WaitingOn != "" {
			t.Fatalf("extract outcome %+v, want only progress and ok", out)
		}
	}
	t.Fatalf("extract still in progress after %d attempts", limit)

	return c
}

func TestExtractSendsAPhotoDoclingBarelyReadToVision(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{readOK("ab c"), readOK("")}}
	v := &fakeDescriber{answers: []describeAnswer{describeOK("A handwritten shopping list: milk, eggs")}}
	r := visionRig(t, e, v)

	got := runExtract(t, r, docsCapture(r), 10)

	if want := []describeCall{{[]byte("JPG"), "image/jpeg"}}; !reflect.DeepEqual(v.calls, want) {
		t.Fatalf("describer calls %+v, want only the photo %+v", v.calls, want)
	}
	f := got.Files[0]
	if f.State != capture.FileDone || f.TextSource != capture.TextVision ||
		f.Text != "A handwritten shopping list: milk, eggs" {
		t.Errorf("photo %+v, want the vision answer as its text", f)
	}
	if got.Files[1].State != capture.FileEmpty || got.Files[1].TextSource == capture.TextVision {
		t.Errorf("pdf %+v, want empty from docling", got.Files[1])
	}
	if len(e.calls) != 2 {
		t.Errorf("docling calls %d, want 2", len(e.calls))
	}

	// A re-run sends nothing again.
	if _, out := r.run(capture.StepExtract, got); out.Class != capture.ClassOK || len(v.calls) != 1 {
		t.Errorf("re-run: out %+v describer calls %d", out, len(v.calls))
	}
}

func TestExtractKeepsAPhotoDoclingReadWell(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{readOK("Invoice 42, total 12.50 EUR, due Friday")}}
	v := &fakeDescriber{answers: []describeAnswer{describeOK("unused")}}
	r := visionRig(t, e, v)
	r.audio.files["5.jpg"] = []byte("JPG")
	c := filesCapture(capture.File{
		Kind: capture.FileImage, State: capture.FilePending, MimeType: "image/jpeg", LocalPath: "5.jpg",
	})

	got := runExtract(t, r, c, 5)

	if len(v.calls) != 0 {
		t.Errorf("describer calls %d, want none for a photo docling read", len(v.calls))
	}
	if f := got.Files[0]; f.TextSource != capture.TextDocling || f.State != capture.FileDone {
		t.Errorf("photo %+v, want docling's text", f)
	}
}

func TestExtractWithoutDoclingSendsOnlyPhotosToVision(t *testing.T) {
	v := &fakeDescriber{answers: []describeAnswer{describeOK("A cat on a sofa")}}
	r := visionRig(t, nil, v)

	got := runExtract(t, r, docsCapture(r), 10)

	if want := []describeCall{{[]byte("JPG"), "image/jpeg"}}; !reflect.DeepEqual(v.calls, want) {
		t.Fatalf("describer calls %+v, want only the photo %+v", v.calls, want)
	}
	if f := got.Files[0]; f.State != capture.FileDone || f.TextSource != capture.TextVision ||
		f.Text != "A cat on a sofa" {
		t.Errorf("photo %+v", f)
	}
	if got.Files[1].State != capture.FileSkipped || got.Files[2].State != capture.FileSkipped {
		t.Errorf("document and video %+v, want skipped", got.Files[1:])
	}
}

func TestExtractNeverSendsADocumentToVision(t *testing.T) {
	for _, docling := range []bool{false, true} {
		e := &fakeExtractor{answers: []extractAnswer{readOK("")}}
		if !docling {
			e = nil
		}
		v := &fakeDescriber{answers: []describeAnswer{describeOK("unused")}}
		r := visionRig(t, e, v)
		r.audio.files["5.pdf"] = []byte("PDF")
		c := filesCapture(capture.File{
			Kind: capture.FileDocument, State: capture.FilePending, MimeType: "application/pdf", LocalPath: "5.pdf",
		})

		runExtract(t, r, c, 5)

		if len(v.calls) != 0 {
			t.Errorf("docling %v: describer calls %d, want none for a document", docling, len(v.calls))
		}
	}
}

func TestExtractNeverSendsAPhotoDoclingFailedToVision(t *testing.T) {
	e := &fakeExtractor{answers: []extractAnswer{{out: capture.Permanent("unsupported").On(capture.WaitingDocling)}}}
	v := &fakeDescriber{answers: []describeAnswer{describeOK("unused")}}
	r := visionRig(t, e, v)
	r.audio.files["5.jpg"] = []byte("JPG")
	c := filesCapture(capture.File{
		Kind: capture.FileImage, State: capture.FilePending, MimeType: "image/jpeg", LocalPath: "5.jpg",
	})

	got := runExtract(t, r, c, 5)

	if len(v.calls) != 0 || got.Files[0].State != capture.FileFailed {
		t.Errorf("describer calls %d, photo %+v; want no call and the file failed", len(v.calls), got.Files[0])
	}
}

// FR-123: a failing vision model is retried within its budget without ever
// making the capture wait, then the photo is left empty and extract goes on.
func TestExtractVisionFailureDegradesWithoutWaiting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		out   capture.Outcome
		calls int
	}{
		{"temporary", capture.Temporary("vision: 503"), 3},
		{"misconfig", capture.Misconfig("vision: 401"), 3},
		{"permanent", capture.Permanent("vision: 400"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &fakeDescriber{answers: []describeAnswer{{out: tc.out}}}
			r := visionRig(t, nil, v)
			r.audio.files["5.jpg"] = []byte("JPG")
			c := filesCapture(capture.File{
				Kind: capture.FileImage, State: capture.FilePending, MimeType: "image/jpeg", LocalPath: "5.jpg",
			})

			got := runExtract(t, r, c, 10)

			if len(v.calls) != tc.calls {
				t.Errorf("describer calls %d, want %d", len(v.calls), tc.calls)
			}
			f := got.Files[0]
			if f.State != capture.FileEmpty || f.Text != "" || f.VisionAttempts != tc.calls {
				t.Errorf("photo %+v, want empty after %d attempts", f, tc.calls)
			}
			if got.Failure != "" || got.WaitingOn != "" {
				t.Errorf("capture failure %q waiting on %q, want neither", got.Failure, got.WaitingOn)
			}
		})
	}
}

// An OK but empty answer leaves the photo empty and counts the attempt, so a
// re-run without docling does not send it again.
func TestExtractEmptyVisionAnswerLeavesThePhotoEmpty(t *testing.T) {
	v := &fakeDescriber{answers: []describeAnswer{describeOK("")}}
	r := visionRig(t, nil, v)
	r.audio.files["5.jpg"] = []byte("JPG")
	c := filesCapture(capture.File{
		Kind: capture.FileImage, State: capture.FilePending, MimeType: "image/jpeg", LocalPath: "5.jpg",
	})

	got := runExtract(t, r, c, 5)

	if f := got.Files[0]; f.State != capture.FileEmpty || f.VisionAttempts == 0 || f.TextSource == capture.TextVision {
		t.Errorf("photo %+v, want empty with the attempt counted", f)
	}
	if _, out := r.run(capture.StepExtract, got); out.Class != capture.ClassOK || len(v.calls) != 1 {
		t.Errorf("re-run: out %+v describer calls %d, want ok and no new call", out, len(v.calls))
	}
}

func TestExtractVisionMissingFileFailsThePhoto(t *testing.T) {
	v := &fakeDescriber{answers: []describeAnswer{describeOK("unused")}}
	r := visionRig(t, nil, v)
	c := filesCapture(capture.File{
		Kind: capture.FileImage, State: capture.FilePending, MimeType: "image/jpeg", LocalPath: "5.jpg",
	})

	got := runExtract(t, r, c, 5)

	if f := got.Files[0]; f.State != capture.FileFailed || f.LocalPath != "" || len(v.calls) != 0 {
		t.Errorf("photo %+v calls %d, want failed without a call", f, len(v.calls))
	}
}
