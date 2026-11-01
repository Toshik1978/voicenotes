package pipeline

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/enrich"
	"github.com/Toshik1978/voicenotes/internal/language"
	"github.com/Toshik1978/voicenotes/internal/render"
)

// ErrFileTooLarge is returned (possibly wrapped) by a Downloader for a file
// the source refuses to serve; no retry can fetch it. telegram.ErrTooLarge is
// this value.
var ErrFileTooLarge = errors.New("pipeline: file too large to download")

// ErrRejected is wrapped (with %w) by an adapter for a request the source
// refused for good, such as Telegram 400 or 403. Retrying cannot succeed, so
// the queue must not wait on it.
var ErrRejected = errors.New("pipeline: request rejected")

// TranscribeRequest and TranscribeResult mirror the speech-to-text adapter's
// call; the wiring adapts the real client to Transcriber.
type TranscribeRequest struct {
	ID string
	// Ext is the stored audio file's extension (for example ".mp3"), so the
	// service can pick the decoder; empty means Ogg.
	Ext      string
	Audio    []byte
	Language string
}

// TranscribeResult is the transcript and the language the service reported.
type TranscribeResult struct {
	Text     string
	Language string
}

// Transcriber turns audio into text.
type Transcriber interface {
	Transcribe(ctx context.Context, r TranscribeRequest) (TranscribeResult, capture.Outcome)
}

// NoteInput is one note to write; Content is already rendered.
type NoteInput struct {
	CaptureID  int64
	Content    string
	ReceivedAt time.Time
}

// NotesWriter writes notes. Create must be idempotent per capture and return
// the note name and link; Attach must not duplicate an attachment.
type NotesWriter interface {
	Create(ctx context.Context, n NoteInput) (name, link string, out capture.Outcome)
	Attach(ctx context.Context, name, filename, mime string, data []byte) capture.Outcome
}

// Replier sends and edits silent capture replies; Edit falls back to a new
// reply when the message cannot be edited and returns the holding ID.
type Replier interface {
	Send(ctx context.Context, chatID, replyTo int64, text string) (int64, error)
	Edit(ctx context.Context, chatID, msgID int64, text string) (int64, error)
}

// Downloader fetches a file's bytes by its source file ID.
type Downloader interface {
	Download(ctx context.Context, fileID string) ([]byte, error)
}

// FileStore holds capture files by name, relative to the files directory.
// Write is durable and replaces an existing file; Remove of a missing file
// is not an error.
type FileStore interface {
	Write(name string, data []byte) error
	Read(name string) ([]byte, error)
	Exists(name string) (bool, error)
	Remove(name string) error
}

// Enricher is the chat call of the optional LLM step. It is nil when the LLM
// is not configured.
type Enricher interface {
	Complete(ctx context.Context, messages []enrich.Message) (string, capture.Outcome)
}

// ExtractResult mirrors the document reader's result: Markdown is meaningful
// only with an OK outcome and may be empty. TimedOut reports that the reader
// gave no final status in time; the outcome is then Temporary and extract
// applies the per-file timeout policy.
type ExtractResult struct {
	Markdown string
	TimedOut bool
}

// Extractor reads the text of one image or document. It is nil when the
// document reader is not configured; the wiring adapts the docling client.
type Extractor interface {
	Convert(ctx context.Context, name, mime string, data []byte) (ExtractResult, capture.Outcome)
}

// Deps are the collaborators of the step handlers. All but Enricher,
// Extractor and Log are required.
type Deps struct {
	Transcriber Transcriber
	Notes       NotesWriter
	Replier     Replier
	Downloader  Downloader
	Files       FileStore
	Enricher    Enricher
	Extractor   Extractor
	Log         *zap.Logger
}

// StepsConfig carries the configuration the handlers need.
type StepsConfig struct {
	InboxTag        string
	TodoTag         string
	NoteLimit       int
	SourceLanguages []string
	// Location decides the local date in attachment names; nil means UTC.
	Location *time.Location
	// Enrich selects the LLM functions and validation limits; it is used only
	// when Deps.Enricher is set.
	Enrich enrich.Options
}

// NewHandlers builds the handler for every step. Each handler is safe to
// re-run after a crash (Constitution VI): see the per-step notes.
func NewHandlers(d Deps, cfg StepsConfig) (map[capture.Step]Handler, error) {
	switch {
	case d.Transcriber == nil:
		return nil, errors.New("pipeline: nil transcriber")
	case d.Notes == nil:
		return nil, errors.New("pipeline: nil notes writer")
	case d.Replier == nil:
		return nil, errors.New("pipeline: nil replier")
	case d.Downloader == nil:
		return nil, errors.New("pipeline: nil downloader")
	case d.Files == nil:
		return nil, errors.New("pipeline: nil file store")
	}
	if d.Log == nil {
		d.Log = zap.NewNop()
	}
	s := &stepRunner{d: d, cfg: cfg}

	return map[capture.Step]Handler{
		capture.StepCollect:       s.collect,
		capture.StepDownload:      s.download,
		capture.StepExtract:       s.extract,
		capture.StepEnrich:        s.enrich,
		capture.StepWriteNote:     s.writeNote,
		capture.StepFinalizeReply: s.finalizeReply,
		capture.StepCleanup:       s.cleanup,
	}, nil
}

// attachFailedMark prefixes Capture.Failure when the note was saved with its
// text but without the audio (a minimal note after a permanent attach
// failure). Failure survives until the reply is sent, unlike LastError, and
// replyText turns the marked value into the "audio not attached" reply.
const attachFailedMark = "audio not attached: "

// textPending is the acknowledgement reply (contracts/telegram-ux.md).
const textPending = "⏳"

type stepRunner struct {
	d   Deps
	cfg StepsConfig
}

func (s *stepRunner) log(c capture.Capture, msg string, err error) {
	fields := []zap.Field{zap.Int64("capture", c.ID)}
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	s.d.Log.Warn(msg, fields...)
}

// download stores each file locally, one per attempt, and only after the last
// one acknowledges with the hourglass. Progress is returned while files remain.
// Re-running reuses a file that is already on disk and does not resend the
// hourglass once a reply id is recorded. A failed hourglass never fails the
// step: finalize_reply sends a new reply instead. A file the source refuses
// for good is marked failed; for an audio capture, which holds that one file,
// the step fails permanently and the capture degrades to a placeholder note.
func (s *stepRunner) download(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	c.Files = slices.Clone(c.Files)
	i, out := s.nextDownload(c)
	if out.Class != capture.ClassOK {
		return c, out
	}
	if i >= 0 {
		if out = s.downloadFile(ctx, &c, i); out.Class != capture.ClassOK {
			return c, out
		}
		if slices.ContainsFunc(c.Files[i+1:], wantsDownload) {
			return c, capture.Progress()
		}
	}

	if c.ReplyMessageID == 0 {
		id, err := s.d.Replier.Send(ctx, c.SourceChatID, c.SourceMessageID, textPending)
		if err != nil {
			s.log(c, "acknowledgement failed, finalize will send a new reply", err)
		} else {
			c.ReplyMessageID = id
		}
	}

	return c, capture.OK()
}

// wantsDownload reports whether download must make sure f is held locally.
func wantsDownload(f capture.File) bool { return f.State == capture.FilePending }

// nextDownload returns the index of the first file that is still pending and
// not on disk, or -1 when none is.
func (s *stepRunner) nextDownload(c capture.Capture) (int, capture.Outcome) {
	for i := range c.Files {
		f := c.Files[i]
		if !wantsDownload(f) {
			continue
		}
		if f.LocalPath == "" {
			return i, capture.OK()
		}
		ok, err := s.d.Files.Exists(f.LocalPath)
		if err != nil {
			s.log(c, "file check failed", err)

			return -1, capture.Temporary("file store: " + err.Error())
		}
		if !ok {
			return i, capture.OK()
		}
	}

	return -1, capture.OK()
}

// downloadFile fetches c.Files[i] and stores it durably. A file the source
// refuses for good is marked failed: permanent for an audio capture, OK (the
// batch goes on) for the others.
func (s *stepRunner) downloadFile(ctx context.Context, c *capture.Capture, i int) capture.Outcome {
	f := &c.Files[i]
	data, err := s.d.Downloader.Download(ctx, f.FileID)
	reason := ""
	switch {
	case errors.Is(err, ErrFileTooLarge):
		s.log(*c, "file too large to download", err)
		reason = "too large for Telegram bots"
	case errors.Is(err, ErrRejected):
		s.log(*c, "file download rejected", err)
		reason = "unavailable from Telegram"
	case err != nil:
		s.log(*c, "file download failed", err)

		return capture.Temporary("download: " + err.Error()).On(capture.WaitingTelegram)
	}
	if reason != "" {
		// Nothing is held: the placeholder must say so.
		f.LocalPath, f.State, f.Failure = "", capture.FileFailed, reason
		if c.Kind == capture.KindAudio {
			return capture.Permanent("audio " + reason)
		}

		return capture.OK()
	}
	name := localName(c.ID, *f)
	if err := s.d.Files.Write(name, data); err != nil {
		s.log(*c, "file write failed", err)

		return capture.Temporary("file store: " + err.Error())
	}
	f.LocalPath = name

	return capture.OK()
}

// extract turns the pending files into text, one per attempt, and returns
// Progress while any remain. A file already settled is not sent again. Audio
// goes to speech-to-text; photos and documents go to the document reader when
// one is wired and are skipped otherwise; videos are always skipped. Once none
// remain, an audio capture takes its file's text and language, so enrich and
// render see a voice note as before.
func (s *stepRunner) extract(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	c.Files = slices.Clone(c.Files)
	skipUnreadFiles(c.Files, s.d.Extractor != nil)
	if i := slices.IndexFunc(c.Files, pendingRead); i >= 0 {
		if out := s.extractFile(ctx, &c, i); out.Class != capture.ClassOK {
			return c, out
		}
		if slices.ContainsFunc(c.Files[i+1:], pendingRead) {
			return c, capture.Progress()
		}
	}
	if c.Kind == capture.KindAudio {
		if i := slices.IndexFunc(c.Files, func(f capture.File) bool { return f.Kind == capture.FileAudio }); i >= 0 {
			c.Text, c.Language = c.Files[i].Text, c.Files[i].Language
		}
	}

	return c, capture.OK()
}

// pendingRead reports whether extract still has to read f. Files it cannot
// read were skipped before this is asked.
func pendingRead(f capture.File) bool { return f.State == capture.FilePending }

// extractFile reads c.Files[i] with the reader for its kind.
func (s *stepRunner) extractFile(ctx context.Context, c *capture.Capture, i int) capture.Outcome {
	if c.Files[i].Kind == capture.FileAudio {
		return s.transcribeFile(ctx, c, i)
	}

	return s.readDocument(ctx, c, i)
}

// transcribeFile sends c.Files[i] to speech-to-text. The language is forced
// only when exactly one source language is configured; a reported language
// outside several configured sources is rechecked once. The stored language
// is the normalised ISO-639-1 code, or empty when unknown. An empty transcript
// is a result, not an error. A missing file is permanent: it cannot be
// recovered, so the capture degrades to a placeholder note. A permanent
// outcome marks the file failed.
func (s *stepRunner) transcribeFile(ctx context.Context, c *capture.Capture, i int) capture.Outcome {
	f := &c.Files[i]
	data, err := s.d.Files.Read(f.LocalPath)
	if errors.Is(err, fs.ErrNotExist) {
		s.log(*c, "audio file missing", err)
		// Nothing is held: the placeholder must say so.
		f.LocalPath, f.State, f.Failure = "", capture.FileFailed, "audio file missing"

		return capture.Permanent(f.Failure)
	}
	if err != nil {
		s.log(*c, "audio read failed", err)

		return capture.Temporary("file store: " + err.Error())
	}
	forced, isForced := language.Plan(s.cfg.SourceLanguages)
	req := TranscribeRequest{ID: strconv.FormatInt(c.ID, 10), Ext: path.Ext(f.LocalPath), Audio: data}
	if isForced {
		req.Language = forced
	}
	res, out := s.d.Transcriber.Transcribe(ctx, req)
	// A reported language outside the configured sources is retried once with
	// the first source. The step's result is persisted only after it returns,
	// so a restart between the two calls re-runs the whole step rather than
	// adding a third call; the retry result is final, never checked again.
	if with, again := language.Recheck(s.cfg.SourceLanguages, res.Language); out.Class == capture.ClassOK && again {
		req.Language = with
		res, out = s.d.Transcriber.Transcribe(ctx, req)
	}
	if out.Class != capture.ClassOK {
		if out.Class == capture.ClassPermanent {
			f.State, f.Failure = capture.FileFailed, out.Reason
		}

		return out
	}
	f.Text, f.TextSource, f.Language, f.State = res.Text, capture.TextSTT, "", capture.FileDone
	reported := res.Language
	if strings.TrimSpace(reported) == "" {
		// Nothing reported (json format): a forced language is still known.
		reported = req.Language
	}
	if iso, ok := language.Normalize(reported); ok {
		f.Language = iso
	}

	return capture.OK()
}

// enrich runs the optional LLM step. Without an Enricher it records
// 'disabled'. A stored result (a re-run after a crash) or an empty transcript
// needs no call. A files capture asks for a hint instead (enrichFiles). An
// answer that fails validation is Permanent, which the policy turns into
// 'invalid_output'; the 3 attempt / 2 minute budget is the policy's too.
// Prompts, transcripts and answers are never logged.
func (s *stepRunner) enrich(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	if s.d.Enricher == nil {
		c.LLMSkipped = capture.SkippedDisabled

		return c, capture.OK()
	}
	if c.EnrichedJSON != "" {
		return c, capture.OK()
	}
	if c.Kind == capture.KindFiles {
		return s.enrichFiles(ctx, c)
	}
	if strings.TrimSpace(c.Text) == "" {
		return c, capture.OK()
	}
	raw, out := s.d.Enricher.Complete(ctx, enrich.BuildRequest(s.cfg.Enrich, c.Text, c.Language))
	if out.Class != capture.ClassOK {
		return c, out
	}
	res, err := enrich.Parse(raw, c.Text, c.Language, s.cfg.Enrich)
	if c, out = s.storeResult(c, res, err); out.Class != capture.ClassOK {
		return c, out
	}
	// Only the answer's shape: never the transcript or the answer itself.
	s.d.Log.Debug("LLM answer accepted",
		zap.Int64("capture", c.ID), zap.String("detected", c.Language),
		zap.String("language", res.Language), zap.Bool("translated", res.Translated),
		zap.Int("tasks", len(res.Tasks)), zap.Int("tags", len(res.Tags)),
		zap.Int("input_runes", utf8.RuneCountInString(c.Text)),
		zap.Int("text_runes", utf8.RuneCountInString(res.Text)))

	return c, capture.OK()
}

// storeResult records a parsed LLM answer on the capture. An untranslated
// answer is Temporary, any other parse error Permanent ('invalid_output').
func (s *stepRunner) storeResult(c capture.Capture, res enrich.Result, err error) (capture.Capture, capture.Outcome) {
	if errors.Is(err, enrich.ErrNotTranslated) {
		// A sampled answer can keep the source text; ask again within the
		// LLM budget, which then skips the step as unavailable.
		s.log(c, "LLM answer not translated, retrying", nil)

		return c, capture.Temporary("LLM answer not translated")
	}
	if err != nil {
		s.log(c, "LLM answer rejected", err)

		return c, capture.Permanent("LLM output invalid")
	}
	stored, err := res.Marshal()
	if err != nil {
		s.log(c, "enriched result not stored", err)

		return c, capture.Permanent("LLM output not storable")
	}
	c.EnrichedJSON, c.TaskCount = stored, len(res.Tasks)

	return c, capture.OK()
}

// noteInput maps the capture to the renderer input. A stored LLM result
// supplies the main text, checklist and tags; when it translated, the raw
// transcript becomes the quoted original. A result that cannot be decoded
// degrades to the raw transcript rather than failing the note. A minimal note
// ignores the stored result: the retry must differ from the rejected create.
func (s *stepRunner) noteInput(c capture.Capture) render.Input {
	in := render.Input{
		Text: c.Text, Failure: c.Failure, NoAudio: !holdsAudio(c),
		InboxTag: s.cfg.InboxTag, TodoTag: s.cfg.TodoTag,
	}
	if c.EnrichedJSON == "" || c.Failure != "" || c.MinimalNote {
		return in
	}
	res, err := enrich.Unmarshal(c.EnrichedJSON)
	if err != nil {
		s.log(c, "enriched result unreadable, rendering the transcript", err)

		return in
	}
	in.Text, in.Actions, in.SuggestedTags = res.Text, res.Tasks, res.Tags
	if res.Translated {
		in.Original, in.OriginalLang = c.Text, c.Language
		if in.OriginalLang == "" {
			in.OriginalLang = res.Language
		}
	}

	return in
}

// writeNote renders and creates the note (Create is idempotent through its
// deterministic memo id) and then attaches the held files, one per attempt
// (Attach dedupes), returning Progress while any remain. The note name is
// kept even when an attach fails, so a retry resumes there without creating
// the note again; each attached file is recorded, so a re-run never sends it
// twice. MinimalNote keeps the raw text (truncated to the limit) and drops the
// attachments and the LLM output. A files capture has its own flow.
func (s *stepRunner) writeNote(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	c.Files = slices.Clone(c.Files)
	if c.Kind == capture.KindFiles {
		return s.writeFilesNote(ctx, c)
	}
	if c.NoteName == "" || c.MinimalNote {
		var out capture.Outcome
		if c, out = s.createNote(ctx, c); out.Class != capture.ClassOK {
			return c, out
		}
	}
	if c.MinimalNote {
		return c, capture.OK()
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

// createNote renders and creates the note and records its name and link.
func (s *stepRunner) createNote(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	in := s.noteInput(c)
	if c.MinimalNote && holdsFile(c) && c.Failure == "" {
		// The full note was rejected (typically the attachment): the text
		// stays in the note, the audio does not, and the reply says so.
		// LastError holds the reason recorded by the failed attempt.
		reason := c.LastError
		if reason == "" {
			reason = "note rejected"
		}
		c.Failure = attachFailedMark + reason
		c.LastError = ""
	}
	if strings.HasPrefix(c.Failure, attachFailedMark) {
		in.Failure = ""
	}

	return s.create(ctx, c, in)
}

// create renders in, creates the note and records its name and link.
func (s *stepRunner) create(
	ctx context.Context,
	c capture.Capture,
	in render.Input,
) (capture.Capture, capture.Outcome) {
	s.logRendered(c, in)
	name, link, out := s.d.Notes.Create(ctx, NoteInput{
		CaptureID:  c.ID,
		Content:    render.Render(in, s.cfg.NoteLimit),
		ReceivedAt: c.ReceivedAt,
	})
	if out.Class != capture.ClassOK {
		c.LastError = out.Reason

		return c, out
	}
	c.NoteName, c.NoteURL, c.TaskCount = name, link, len(in.Actions)

	return c, capture.OK()
}

// attachable reports whether f is held locally and not yet attached.
func attachable(f capture.File) bool { return f.LocalPath != "" && !f.Attached }

// logRendered records at debug whether the note carries the LLM result, so a
// live deploy can tell a dropped result from an answer that kept the text.
func (s *stepRunner) logRendered(c capture.Capture, in render.Input) {
	main := in.Text
	if len(in.Files) > 0 {
		main = in.Caption
	}
	llm := main != c.Text || len(in.Actions) > 0 || len(in.SuggestedTags) > 0
	s.d.Log.Debug("note rendered",
		zap.Int64("capture", c.ID), zap.Bool("stored_result", c.EnrichedJSON != ""),
		zap.Bool("llm", llm), zap.Bool("minimal", c.MinimalNote), zap.Bool("failure", in.Failure != ""),
		zap.Int("actions", len(in.Actions)))
}

// attach uploads c.Files[i] to the note and records it as attached. A missing
// file is not an error: the note exists and the file cannot be recovered. In
// a files capture a file the notes store refuses for good is recorded as
// attached with its reason, and the note stands (FR-126).
func (s *stepRunner) attach(ctx context.Context, c *capture.Capture, i int) capture.Outcome {
	f := &c.Files[i]
	data, err := s.d.Files.Read(f.LocalPath)
	if errors.Is(err, fs.ErrNotExist) {
		s.log(*c, "file missing, note saved without attachment", err)
		f.LocalPath = "" // the reply must not claim the file was kept

		return capture.OK()
	}
	if err != nil {
		s.log(*c, "file read failed", err)

		return capture.Temporary("file store: " + err.Error())
	}
	out := s.d.Notes.Attach(ctx, c.NoteName, s.attachmentName(*c, *f), attachMime(*f), data)
	if out.Class == capture.ClassPermanent && c.Kind == capture.KindFiles {
		s.log(*c, "file not attached, note saved without it", errors.New(out.Reason))
		f.Attached, f.AttachFailure = true, out.Reason

		return capture.OK()
	}
	if out.Class != capture.ClassOK {
		// Policy.Next drops the reason of a permanent outcome; the minimal
		// rerun reads it back from LastError.
		c.LastError = out.Reason

		return out
	}
	f.Attached = true

	return capture.OK()
}

// finalizeReply edits the hourglass to the result. With no reply to edit, or
// SendNewReply set, it sends a new silent reply. The id of the message that
// now holds the text is recorded.
func (s *stepRunner) finalizeReply(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	return s.reply(ctx, c)
}

func (s *stepRunner) reply(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	text := replyText(c)
	var (
		id  int64
		err error
	)
	if c.ReplyMessageID == 0 || c.SendNewReply {
		id, err = s.d.Replier.Send(ctx, c.SourceChatID, c.SourceMessageID, text)
	} else {
		id, err = s.d.Replier.Edit(ctx, c.SourceChatID, c.ReplyMessageID, text)
	}
	if errors.Is(err, ErrRejected) {
		// No retry can deliver this reply; the note exists, so give up on
		// the reply instead of blocking the queue.
		s.log(c, "reply rejected, giving up on it", err)
		c.SendNewReply = false

		return c, capture.OK()
	}
	if err != nil {
		s.log(c, "reply failed", err)

		return c, capture.Temporary("telegram: " + err.Error()).On(capture.WaitingTelegram)
	}
	c.ReplyMessageID, c.SendNewReply = id, false

	return c, capture.OK()
}

// QueueNotifier edits the reply of a capture that has just gone into waiting
// and returns the capture with QueueReplyEdit cleared and ReplyMessageID
// updated. It never fails: the edit is cosmetic, so an error is logged and
// dropped rather than retried (finalize_reply replaces the text anyway).
type QueueNotifier func(ctx context.Context, c capture.Capture) capture.Capture

// NewQueueNotifier builds the QueueNotifier that edits the hourglass to the
// queued text. A capture with no reply yet has nothing to edit; download
// sends the hourglass later and finalize_reply covers the rest. A nil log
// means no logging.
func NewQueueNotifier(r Replier, log *zap.Logger) QueueNotifier {
	if log == nil {
		log = zap.NewNop()
	}

	return func(ctx context.Context, c capture.Capture) capture.Capture {
		c.QueueReplyEdit = false
		if c.ReplyMessageID == 0 {
			return c
		}
		id, err := r.Edit(ctx, c.SourceChatID, c.ReplyMessageID, QueuedText(c.WaitingOn))
		if err != nil {
			log.Warn("queued notice failed, skipping it", zap.Int64("capture", c.ID), zap.Error(err))

			return c
		}
		c.ReplyMessageID = id

		return c
	}
}

// QueuedText is the reply for a capture waiting on a dependency
// (contracts/telegram-ux.md). An unknown dependency drops the name.
func QueuedText(on capture.WaitingOn) string {
	switch on {
	case capture.WaitingSTT:
		return "⏳ queued — speech-to-text unavailable, will retry"
	case capture.WaitingMemos:
		return "⏳ queued — notes unavailable, will retry"
	case capture.WaitingTelegram:
		return "⏳ queued — Telegram unavailable, will retry"
	case capture.WaitingDocling:
		return "⏳ queued — document reader unavailable, will retry"
	}

	return "⏳ queued — will retry"
}

// cleanup removes every local file and the content (FR-012); the identity
// needed for dedupe stays. Policy.Next clears SendNewReply when
// finalize_reply fails permanently and the step advances, so the new reply it
// asked for is delivered here, before anything is erased.
func (s *stepRunner) cleanup(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	if c.SendNewReply {
		var out capture.Outcome
		if c, out = s.reply(ctx, c); out.Class != capture.ClassOK {
			return c, out
		}
	}
	c.Files = slices.Clone(c.Files)
	for i := range c.Files {
		f := &c.Files[i]
		if f.LocalPath != "" {
			if err := s.d.Files.Remove(f.LocalPath); err != nil {
				s.log(c, "file remove failed", err)

				return c, capture.Temporary("file store: " + err.Error())
			}
		}
		f.LocalPath, f.FileID, f.FileName, f.Text = "", "", "", ""
	}
	c.Text, c.Language, c.EnrichedJSON = "", "", ""

	return c, capture.OK()
}

// holdsFile reports whether a local copy of any file is known.
func holdsFile(c capture.Capture) bool {
	return slices.ContainsFunc(c.Files, func(f capture.File) bool { return f.LocalPath != "" })
}

// holdsAudio reports whether the note carries (or will carry) the audio: a
// local file is known and the note is not a minimal one, which drops it.
func holdsAudio(c capture.Capture) bool {
	return holdsFile(c) && !c.MinimalNote
}

// replyText builds the final reply (contracts/telegram-ux.md), HTML-escaped.
func replyText(c capture.Capture) string {
	link := `<a href="` + html.EscapeString(c.NoteURL) + `">open</a>`
	if c.Kind == capture.KindFiles {
		return filesReplyText(c, link)
	}
	if reason, ok := strings.CutPrefix(c.Failure, attachFailedMark); ok {
		return fmt.Sprintf("⚠ saved, audio not attached — %s · %s", html.EscapeString(reason), link)
	}
	if c.Failure != "" && !holdsAudio(c) {
		// Neither text nor audio was kept: ask the owner to send it again,
		// since Telegram never redelivers it.
		return fmt.Sprintf("⚠ audio not kept — %s · please send it again · %s", html.EscapeString(c.Failure), link)
	}
	if c.Failure != "" {
		return fmt.Sprintf("⚠ saved audio only — %s · %s", html.EscapeString(c.Failure), link)
	}

	return savedText(c, link)
}

// savedText is the reply for a note saved in full, with the "(no AI)" and
// task-count marks.
func savedText(c capture.Capture, link string) string {
	head := "✓ saved"
	if c.LLMSkipped == capture.SkippedUnavailable || c.LLMSkipped == capture.SkippedInvalidOutput {
		head += " (no AI)"
	}
	switch {
	case c.TaskCount == 1:
		head += " · 1 task"
	case c.TaskCount > 1:
		head += fmt.Sprintf(" · %d tasks", c.TaskCount)
	}

	return head + " · " + link
}

const (
	extOgg  = ".ogg"
	extMP3  = ".mp3"
	extM4A  = ".m4a"
	extWAV  = ".wav"
	extFLAC = ".flac"
	extWebM = ".webm"
)

// localName is "<id>.<ext>" for the first file and "<id>-<position>.<ext>"
// for the others. An audio file's extension comes from its MIME type (Ogg
// unless it says otherwise), any other file's from its original name.
func localName(captureID int64, f capture.File) string {
	name := strconv.FormatInt(captureID, 10)
	if f.Position > 0 {
		name += "-" + strconv.Itoa(f.Position)
	}
	if f.Kind != capture.FileAudio {
		return name + SanitizeExt(path.Ext(f.FileName))
	}

	return name + audioExt(f.MimeType)
}

func audioExt(mime string) string {
	switch mime {
	case "audio/mpeg", "audio/mp3":
		return extMP3
	case "audio/mp4", "audio/m4a", "audio/x-m4a", "audio/aac":
		return extM4A
	case "audio/wav", "audio/x-wav", "audio/wave":
		return extWAV
	case "audio/flac", "audio/x-flac":
		return extFLAC
	case "audio/webm":
		return extWebM
	}

	return extOgg
}

// attachmentName is the capture's Memos attachment filename for f, from
// AttachmentNames over the whole capture so that names stay unique and stable.
func (s *stepRunner) attachmentName(c capture.Capture, f capture.File) string {
	names := AttachmentNames(c.ReceivedAt, s.cfg.Location, c.Files)
	for i := range c.Files {
		if c.Files[i].Position == f.Position {
			return names[i]
		}
	}

	return AttachmentNames(c.ReceivedAt, s.cfg.Location, []capture.File{f})[0]
}

// audioMime is the attachment MIME type for a stored file name.
func audioMime(name string) string {
	switch path.Ext(name) {
	case extMP3:
		return "audio/mpeg"
	case extM4A:
		return "audio/mp4"
	case extWAV:
		return "audio/wav"
	case extFLAC:
		return "audio/flac"
	case extWebM:
		return "audio/webm"
	}

	return "audio/ogg"
}
