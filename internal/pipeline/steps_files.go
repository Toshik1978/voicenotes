package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

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

// skipUnreadFiles marks every pending video skipped, since a video is never
// read, every pending document when no document reader is wired, and every
// pending photo when neither a document reader nor a vision model is. Audio
// and files already settled (failed downloads among them) are left alone.
func skipUnreadFiles(files []capture.File, reader, vision bool) {
	for i := range files {
		f := &files[i]
		if f.State != capture.FilePending {
			continue
		}
		switch f.Kind {
		case capture.FileVideo:
			f.State = capture.FileSkipped
		case capture.FileDocument:
			if !reader {
				f.State = capture.FileSkipped
			}
		case capture.FileImage:
			if !reader && !vision {
				f.State = capture.FileSkipped
			}
		case capture.FileAudio:
		}
	}
}

// readHeld reads the local copy of c.Files[i]. A missing copy fails the file
// (it cannot be recovered) and returns ok=false with an OK outcome, so the
// batch goes on; any other read error is Temporary.
func (s *stepRunner) readHeld(c *capture.Capture, i int) ([]byte, capture.Outcome, bool) {
	f := &c.Files[i]
	data, err := s.d.Files.Read(f.LocalPath)
	if errors.Is(err, fs.ErrNotExist) {
		s.log(*c, "file missing, not read", err)
		// Nothing is held: the placeholder must say so.
		f.LocalPath, f.State, f.Failure = "", capture.FileFailed, "file missing"

		return nil, capture.OK(), false
	}
	if err != nil {
		s.log(*c, "file read failed", err)

		return nil, capture.Temporary("file store: " + err.Error()), false
	}

	return data, capture.OK(), true
}

// readDocument sends the photo or document c.Files[i] to the document reader
// and records its text. A permanent failure, or the timeout that ends the
// file's budget, fails the file and the batch goes on (OK). Any other failure
// keeps the file pending and waits on the reader. A missing local copy fails
// the file: it cannot be recovered. Content is never logged.
func (s *stepRunner) readDocument(ctx context.Context, c *capture.Capture, i int) capture.Outcome {
	data, out, ok := s.readHeld(c, i)
	if !ok {
		return out
	}
	f := &c.Files[i]
	name := cmp.Or(SanitizeFileName(f.FileName), path.Base(f.LocalPath))
	start := time.Now()
	res, out := s.d.Extractor.Convert(ctx, name, f.MimeType, data)
	s.d.Log.Debug("document read",
		zap.Int64("capture", c.ID), zap.Int("position", f.Position), zap.String("kind", f.Kind.String()),
		zap.Int("bytes", len(data)), zap.Duration("took", time.Since(start)),
		zap.Stringer("class", out.Class), zap.Bool("timed_out", res.TimedOut),
		zap.Int("text_bytes", len(res.Markdown)))
	if res.TimedOut {
		out = capture.DoclingTimeout(f)
	}
	if out.Class == capture.ClassPermanent {
		f.State, f.Failure = capture.FileFailed, out.Reason

		return capture.OK()
	}
	if out.Class != capture.ClassOK {
		return out.On(capture.WaitingDocling)
	}
	f.Text, f.TextSource, f.State = res.Markdown, capture.TextDocling, capture.FileDone
	if strings.TrimSpace(res.Markdown) == "" {
		f.Text, f.State = "", capture.FileEmpty
	}

	return capture.OK()
}

// describePhoto sends the photo c.Files[i] to the vision model and records
// its answer as the file's text. The vision model never makes the capture
// wait (FR-123): a failure within the per-file budget is recorded and
// returned as Progress, so the next attempt retries at once without a waiting
// state or a queued reply; a permanent failure, an empty answer or a spent
// budget leaves the photo empty and the batch goes on (OK). Content is never
// logged.
func (s *stepRunner) describePhoto(ctx context.Context, c *capture.Capture, i int) capture.Outcome {
	data, out, ok := s.readHeld(c, i)
	if !ok {
		return out
	}
	f := &c.Files[i]
	start := time.Now()
	text, out := s.d.Describer.Describe(ctx, data, f.MimeType)
	s.d.Log.Debug("photo described",
		zap.Int64("capture", c.ID), zap.Int("position", f.Position),
		zap.Int("bytes", len(data)), zap.Duration("took", time.Since(start)),
		zap.Stringer("class", out.Class), zap.Int("text_bytes", len(text)))
	if out.Class == capture.ClassOK && strings.TrimSpace(text) == "" {
		out = capture.Permanent("vision: empty answer")
	}
	if out.Class != capture.ClassOK {
		if capture.VisionFailed(f, out, time.Now()).Class != capture.ClassOK {
			return capture.Progress()
		}

		return capture.OK()
	}
	f.Text, f.TextSource, f.State = text, capture.TextVision, capture.FileDone

	return capture.OK()
}

// enrichFiles asks the LLM for one hint over a files capture: its caption,
// if any, and an excerpt of every file with text, bounded in total by
// Options.ExcerptRunes. With neither there is nothing to describe and no
// call; the renderer's fallback hint applies. Skip, budget and invalid-output
// rules are enrich's. Only sizes are logged, never the caption, the document
// text or the answer.
func (s *stepRunner) enrichFiles(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	excerpts := s.fileExcerpts(c)
	if len(excerpts) == 0 && strings.TrimSpace(c.Text) == "" {
		return c, capture.OK()
	}
	raw, out := s.d.Enricher.Complete(ctx, enrich.BuildFilesRequest(s.cfg.Enrich, c.Text, excerpts))
	if out.Class != capture.ClassOK {
		return c, out
	}
	res, err := enrich.ParseFiles(raw, c.Text, c.Language, s.cfg.Enrich)
	if c, out = s.storeResult(c, res, err); out.Class != capture.ClassOK {
		return c, out
	}
	excerptRunes := 0
	for _, e := range excerpts {
		excerptRunes += utf8.RuneCountInString(e.Text)
	}
	s.d.Log.Debug("LLM hint accepted",
		zap.Int64("capture", c.ID), zap.String("detected", c.Language),
		zap.String("language", res.Language), zap.Bool("translated", res.Translated),
		zap.Int("tasks", len(res.Tasks)), zap.Int("tags", len(res.Tags)),
		zap.Int("files_with_text", len(excerpts)), zap.Int("excerpt_runes", excerptRunes),
		zap.Int("caption_runes", utf8.RuneCountInString(c.Text)),
		zap.Int("hint_runes", utf8.RuneCountInString(res.Hint)))

	return c, capture.OK()
}

// fileExcerpts lists, in file order, the text of every file that has some,
// labelled with the file's attachment name (a photo has no name of its own).
// BuildFilesRequest cuts them to the budget.
func (s *stepRunner) fileExcerpts(c capture.Capture) []enrich.Excerpt {
	names := AttachmentNames(c.ReceivedAt, s.cfg.Location, c.Files)
	out := []enrich.Excerpt{}
	for i := range c.Files {
		if text := strings.TrimSpace(c.Files[i].Text); text != "" {
			out = append(out, enrich.Excerpt{Name: names[i], Text: text})
		}
	}

	return out
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

// filesNoteInput maps a files capture to the renderer input. A stored LLM
// result supplies the processed caption, the hint, the checklist, quote and
// tags; without one (or without a hint in it) the hint falls back to
// render.FallbackHint over every file, whose failure reasons it names.
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
	if res.Hint != "" {
		in.Hint = res.Hint
	}
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
