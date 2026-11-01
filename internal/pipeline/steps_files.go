package pipeline

import (
	"cmp"
	"context"
	"fmt"
	"html"
	"slices"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/enrich"
	"github.com/Toshik1978/voicenotes/internal/render"
)

// mimeOctetStream is the attachment MIME type of a file whose type is unknown.
const mimeOctetStream = "application/octet-stream"

// collect ends a files capture's batching window. The capture only becomes
// due once its quiet period has passed, so there is nothing left to do.
func (s *stepRunner) collect(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	return c, capture.OK()
}

// skipUnreadFiles marks every pending photo, document and video skipped: no
// document reader is wired yet, and a video is never read. Audio and files
// already settled (failed downloads among them) are left alone.
func skipUnreadFiles(files []capture.File) {
	for i := range files {
		f := &files[i]
		if f.Kind != capture.FileAudio && f.State == capture.FilePending {
			f.State = capture.FileSkipped
		}
	}
}

// writeFilesNote creates the note of a files capture once and then attaches
// its held files in order, one per attempt, returning Progress while any
// remain. A file the notes store refuses for good is recorded on the file and
// the note stands (FR-126). The audio minimal-note path does not apply: a
// retry after a rejected create only drops the LLM output.
func (s *stepRunner) writeFilesNote(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	if c.NoteName == "" {
		var out capture.Outcome
		if c, out = s.create(ctx, c, s.filesNoteInput(c)); out.Class != capture.ClassOK {
			return c, out
		}
	}
	i := slices.IndexFunc(c.Files, attachable)
	if i < 0 {
		return c, capture.OK()
	}
	if out := s.attach(ctx, &c, i); out.Class != capture.ClassOK {
		return c, out
	}
	if slices.ContainsFunc(c.Files[i+1:], attachable) {
		return c, capture.Progress()
	}

	return c, capture.OK()
}

// filesNoteInput maps a files capture to the renderer input: the caption
// (processed by the LLM step when a result is stored), the fallback hint over
// every file, and the checklist, quote and tags of the stored result.
func (s *stepRunner) filesNoteInput(c capture.Capture) render.Input {
	files := fileSummaries(c.Files)
	in := render.Input{
		Files: files, Caption: c.Text, Hint: render.FallbackHint(files),
		InboxTag: s.cfg.InboxTag, TodoTag: s.cfg.TodoTag,
	}
	if c.EnrichedJSON == "" || c.MinimalNote {
		return in
	}
	res, err := enrich.Unmarshal(c.EnrichedJSON)
	if err != nil {
		s.log(c, "enriched result unreadable, rendering the caption", err)

		return in
	}
	in.Caption, in.Actions, in.SuggestedTags = res.Text, res.Tasks, res.Tags
	if res.Translated {
		in.Original, in.OriginalLang = c.Text, c.Language
		if in.OriginalLang == "" {
			in.OriginalLang = res.Language
		}
	}

	return in
}

// fileSummaries converts the capture's files to what the renderer reads.
// render's kinds and states are plain strings equal to the capture values.
func fileSummaries(files []capture.File) []render.FileSummary {
	out := make([]render.FileSummary, len(files))
	for i := range files {
		f := &files[i]
		out[i] = render.FileSummary{
			Kind: f.Kind.String(), Name: SanitizeFileName(f.FileName), State: f.State.String(),
			Failure: f.Failure, Text: f.Text,
		}
	}

	return out
}

// attachMime is the attachment MIME type of f: audio keeps the type derived
// from its stored name, any other file its own type or octet-stream.
func attachMime(f capture.File) string {
	if f.Kind == capture.FileAudio {
		return audioMime(f.LocalPath)
	}
	if f.MimeType == "" {
		return mimeOctetStream
	}

	return f.MimeType
}

// filesReplyText builds the final reply of a files capture
// (contracts/telegram-ux.md). "Not kept" wins over "not attached", and the
// reason named is the first such failure's.
func filesReplyText(c capture.Capture, link string) string {
	t := tallyFiles(c.Files)
	n := len(c.Files)
	switch {
	case n > 0 && t.notKept == n:
		return fmt.Sprintf("⚠ files not kept — %s · please send them again · %s",
			html.EscapeString(t.keptReason), link)
	case t.notKept > 0:
		return fmt.Sprintf("⚠ saved, %d of %d not kept — %s · %s",
			t.notKept, n, html.EscapeString(t.keptReason), link)
	case t.notAttached > 0:
		return fmt.Sprintf("⚠ saved, %d of %d not attached — %s · %s",
			t.notAttached, n, html.EscapeString(t.attachReason), link)
	}

	return savedText(c, link)
}

// fileTally counts the files the reply must report, with the first reason
// of each kind.
type fileTally struct {
	notKept, notAttached     int
	keptReason, attachReason string
}

// tallyFiles counts the files with no local copy (not kept: the download
// failed, or the copy went missing) and those the notes store refused (not
// attached).
func tallyFiles(files []capture.File) fileTally {
	var t fileTally
	for i := range files {
		f := &files[i]
		switch {
		case f.LocalPath == "":
			t.notKept++
			if t.keptReason == "" {
				t.keptReason = cmp.Or(f.Failure, "file missing")
			}
		case f.AttachFailure != "":
			t.notAttached++
			if t.attachReason == "" {
				t.attachReason = f.AttachFailure
			}
		}
	}

	return t
}
