package pipeline_test

import (
	"testing"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// twoFiles is a files capture holding two documents, neither downloaded.
func twoFiles() capture.Capture {
	c := base()
	c.Kind = capture.KindFiles
	c.Files = []capture.File{
		{ID: 1, Position: 0, Kind: capture.FileDocument, State: capture.FilePending, FileID: "A", FileName: "a.pdf"},
		{ID: 2, Position: 1, Kind: capture.FileDocument, State: capture.FilePending, FileID: "B", FileName: "b.pdf"},
	}

	return c
}

func TestDownloadOneFilePerAttempt(t *testing.T) {
	r := newRig(t)
	c := twoFiles()

	got, out := r.run(capture.StepDownload, c)
	if out.Class != capture.ClassProgress {
		t.Fatalf("first attempt: out %+v, want progress", out)
	}
	if r.dl.calls != 1 || got.Files[0].LocalPath == "" || got.Files[1].LocalPath != "" {
		t.Fatalf("first attempt: calls %d files %+v", r.dl.calls, got.Files)
	}
	if len(r.rep.sends) != 0 {
		t.Errorf("hourglass sent before the step finished: %+v", r.rep.sends)
	}
	if c.Files[0].LocalPath != "" {
		t.Error("the handler must not write through to the caller's files")
	}

	got, out = r.run(capture.StepDownload, got)
	if out.Class != capture.ClassOK {
		t.Fatalf("second attempt: out %+v, want ok", out)
	}
	if r.dl.calls != 2 || got.Files[1].LocalPath == "" || got.Files[0].LocalPath == got.Files[1].LocalPath {
		t.Fatalf("second attempt: calls %d files %+v", r.dl.calls, got.Files)
	}
	if len(r.audio.files) != 2 {
		t.Errorf("stored %v", r.audio.files)
	}
	if len(r.rep.sends) != 1 || got.ReplyMessageID != 77 {
		t.Errorf("sends %+v reply %d", r.rep.sends, got.ReplyMessageID)
	}
}

func TestDownloadFailedFile(t *testing.T) {
	t.Run("files capture marks the file failed and goes on", func(t *testing.T) {
		r := newRig(t)
		r.dl.err = pipeline.ErrFileTooLarge
		got, out := r.run(capture.StepDownload, twoFiles())
		if out.Class != capture.ClassProgress {
			t.Fatalf("out %+v, want progress", out)
		}
		if f := got.Files[0]; f.State != capture.FileFailed || f.Failure != "too large for Telegram bots" ||
			f.LocalPath != "" {
			t.Errorf("file %+v", f)
		}
		r.dl.err = nil
		got, out = r.run(capture.StepDownload, got)
		if out.Class != capture.ClassOK || r.dl.calls != 2 || got.Files[1].LocalPath == "" ||
			got.Files[0].State != capture.FileFailed {
			t.Errorf("out %+v calls %d files %+v", out, r.dl.calls, got.Files)
		}
	})
	t.Run("audio capture marks the file failed and is permanent", func(t *testing.T) {
		r := newRig(t)
		r.dl.err = pipeline.ErrRejected
		got, out := r.run(capture.StepDownload, base())
		if out.Class != capture.ClassPermanent {
			t.Fatalf("out %+v", out)
		}
		if f := got.Files[0]; f.State != capture.FileFailed || f.Failure != "unavailable from Telegram" ||
			out.Reason != "audio unavailable from Telegram" {
			t.Errorf("file %+v", f)
		}
	})
}

func TestExtractStoresTheFileResult(t *testing.T) {
	r := newRig(t, "ru")
	r.audio.files["5.ogg"] = []byte("A")
	r.tr.res = pipeline.TranscribeResult{Text: "привет", Language: "russian"}
	c := base()
	c.Files[0].LocalPath = "5.ogg"
	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassOK {
		t.Fatalf("out %+v", out)
	}
	f := got.Files[0]
	if f.State != capture.FileDone || f.Text != "привет" || f.Language != "ru" || f.TextSource != capture.TextSTT {
		t.Errorf("file %+v", f)
	}
	if got.Text != "привет" || got.Language != "ru" {
		t.Errorf("capture text %q language %q", got.Text, got.Language)
	}
	if c.Files[0].State != capture.FilePending {
		t.Error("the handler must not write through to the caller's files")
	}

	again, out := r.run(capture.StepExtract, got)
	if out.Class != capture.ClassOK || r.tr.call != 1 || again.Text != "привет" {
		t.Errorf("rerun: out %+v calls %d text %q", out, r.tr.call, again.Text)
	}
}

func TestExtractPermanentMarksTheFileFailed(t *testing.T) {
	r := newRig(t)
	r.audio.files["5.ogg"] = []byte("A")
	r.tr.out = capture.Permanent("http 415").On(capture.WaitingSTT)
	c := base()
	c.Files[0].LocalPath = "5.ogg"
	got, out := r.run(capture.StepExtract, c)
	if out.Class != capture.ClassPermanent {
		t.Fatalf("out %+v", out)
	}
	if f := got.Files[0]; f.State != capture.FileFailed || f.Failure != "http 415" || f.LocalPath != "5.ogg" {
		t.Errorf("file %+v", f)
	}
}

// heldFiles is a files capture whose two files are on disk and a note exists.
func heldFiles(r *rig) capture.Capture {
	c := twoFiles()
	c.Files[0].LocalPath, c.Files[1].LocalPath = "5.pdf", "5-1.pdf"
	r.audio.files["5.pdf"], r.audio.files["5-1.pdf"] = []byte("PDF-A"), []byte("PDF-B")

	return c
}

func TestWriteNoteAttachesOneFilePerAttempt(t *testing.T) {
	r := newRig(t)
	c := heldFiles(r)

	got, out := r.run(capture.StepWriteNote, c)
	if out.Class != capture.ClassProgress {
		t.Fatalf("first attempt: out %+v, want progress", out)
	}
	if len(r.notes.attached) != 1 || string(r.notes.attached[0].data) != "PDF-A" ||
		!got.Files[0].Attached || got.Files[1].Attached || got.NoteName != "memos/abc" {
		t.Fatalf("first attempt: attached %+v files %+v", r.notes.attached, got.Files)
	}
	if c.Files[0].Attached {
		t.Error("the handler must not write through to the caller's files")
	}

	got, out = r.run(capture.StepWriteNote, got)
	if out.Class != capture.ClassOK {
		t.Fatalf("second attempt: out %+v, want ok", out)
	}
	if len(r.notes.attached) != 2 || string(r.notes.attached[1].data) != "PDF-B" || !got.Files[1].Attached {
		t.Fatalf("second attempt: attached %+v files %+v", r.notes.attached, got.Files)
	}
	if len(r.notes.created) != 1 {
		t.Errorf("the note is created once, then reused: %d creates", len(r.notes.created))
	}
}

// TestWriteNoteRerunSkipsAttachedFiles: a crash after a file's attach was
// persisted resumes with the next file and never re-attaches the first.
func TestWriteNoteRerunSkipsAttachedFiles(t *testing.T) {
	r := newRig(t)
	c := heldFiles(r)
	c.NoteName, c.NoteURL = "memos/abc", "https://memos.example.org/memos/abc"
	c.Files[0].Attached = true

	got, out := r.run(capture.StepWriteNote, c)
	if out.Class != capture.ClassOK {
		t.Fatalf("out %+v", out)
	}
	if len(r.notes.attached) != 1 || string(r.notes.attached[0].data) != "PDF-B" || !got.Files[1].Attached {
		t.Errorf("attached %+v files %+v", r.notes.attached, got.Files)
	}

	_, out = r.run(capture.StepWriteNote, got)
	if out.Class != capture.ClassOK || len(r.notes.attached) != 1 {
		t.Errorf("rerun with everything attached: out %+v attached %d", out, len(r.notes.attached))
	}
}

func TestWriteNoteRecordsTheAudioAttach(t *testing.T) {
	r := newRig(t)
	r.audio.files["5.ogg"] = []byte("A")
	c := base()
	c.Files[0].LocalPath = "5.ogg"
	got, out := r.run(capture.StepWriteNote, c)
	if out.Class != capture.ClassOK || !got.Files[0].Attached {
		t.Errorf("out %+v file %+v", out, got.Files[0])
	}
}

func TestCleanupClearsEveryFile(t *testing.T) {
	r := newRig(t)
	c := heldFiles(r)
	for i := range c.Files {
		c.Files[i].Text = "content"
	}
	got, out := r.run(capture.StepCleanup, c)
	if out.Class != capture.ClassOK || len(r.audio.files) != 0 {
		t.Fatalf("out %+v stored %v", out, r.audio.files)
	}
	for _, f := range got.Files {
		if f.LocalPath != "" || f.FileID != "" || f.FileName != "" || f.Text != "" {
			t.Errorf("file not cleared: %+v", f)
		}
	}
	if c.Files[0].LocalPath == "" {
		t.Error("the handler must not write through to the caller's files")
	}
}

func TestReplyReadsAnyHeldFile(t *testing.T) {
	r := newRig(t)
	c := base()
	c.Files = append(c.Files, capture.File{Kind: capture.FileAudio, State: capture.FileDone, LocalPath: "5-1.ogg"})
	c.ReplyMessageID, c.NoteURL, c.Failure = 9, "https://memos.example.org/x", "http 415"
	r.run(capture.StepFinalizeReply, c)
	want := `⚠ saved audio only — http 415 · <a href="https://memos.example.org/x">open</a>`
	if len(r.rep.edits) != 1 || r.rep.edits[0].text != want {
		t.Errorf("edits %+v", r.rep.edits)
	}
}
