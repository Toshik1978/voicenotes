package pipeline

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
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

// AudioStore holds audio files by name, relative to the audio directory.
// Write is durable and replaces an existing file; Remove of a missing file
// is not an error.
type AudioStore interface {
	Write(name string, data []byte) error
	Read(name string) ([]byte, error)
	Exists(name string) (bool, error)
	Remove(name string) error
}

// Deps are the collaborators of the step handlers. All but Log are required.
type Deps struct {
	Transcriber Transcriber
	Notes       NotesWriter
	Replier     Replier
	Downloader  Downloader
	Audio       AudioStore
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
	case d.Audio == nil:
		return nil, errors.New("pipeline: nil audio store")
	}
	if d.Log == nil {
		d.Log = zap.NewNop()
	}
	s := &stepRunner{d: d, cfg: cfg}

	return map[capture.Step]Handler{
		capture.StepDownload:      s.download,
		capture.StepTranscribe:    s.transcribe,
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

// download stores the audio locally and only then acknowledges with the
// hourglass. Re-running reuses a file that is already on disk and does not
// resend the hourglass once a reply id is recorded. A failed hourglass never
// fails the step: finalize_reply sends a new reply instead.
func (s *stepRunner) download(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	have := false
	if c.AudioPath != "" {
		ok, err := s.d.Audio.Exists(c.AudioPath)
		if err != nil {
			s.log(c, "audio check failed", err)

			return c, capture.Temporary("audio store: " + err.Error())
		}
		have = ok
	}
	if !have {
		data, err := s.d.Downloader.Download(ctx, c.FileID)
		if errors.Is(err, ErrFileTooLarge) {
			s.log(c, "audio too large to download", err)

			return c, capture.Permanent("audio too large for Telegram bots")
		}
		if errors.Is(err, ErrRejected) {
			s.log(c, "audio download rejected", err)

			return c, capture.Permanent("audio unavailable from Telegram")
		}
		if err != nil {
			s.log(c, "audio download failed", err)

			return c, capture.Temporary("download: " + err.Error()).On(capture.WaitingTelegram)
		}
		name := audioName(c)
		if err := s.d.Audio.Write(name, data); err != nil {
			s.log(c, "audio write failed", err)

			return c, capture.Temporary("audio store: " + err.Error())
		}
		c.AudioPath = name
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

// transcribe sends the audio to speech-to-text. The language is forced only
// when exactly one source language is configured. An empty transcript is a
// result, not an error. A missing audio file is permanent: it cannot be
// recovered, so the capture degrades to a placeholder note.
func (s *stepRunner) transcribe(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	data, err := s.d.Audio.Read(c.AudioPath)
	if errors.Is(err, fs.ErrNotExist) {
		s.log(c, "audio file missing", err)

		return c, capture.Permanent("audio file missing")
	}
	if err != nil {
		s.log(c, "audio read failed", err)

		return c, capture.Temporary("audio store: " + err.Error())
	}
	req := TranscribeRequest{ID: strconv.FormatInt(c.ID, 10), Ext: path.Ext(c.AudioPath), Audio: data}
	if len(s.cfg.SourceLanguages) == 1 {
		req.Language = s.cfg.SourceLanguages[0]
	}
	res, out := s.d.Transcriber.Transcribe(ctx, req)
	if out.Class != capture.ClassOK {
		return c, out
	}
	c.Text, c.Language = res.Text, res.Language

	return c, capture.OK()
}

// enrich is the pass-through used until an LLM is configured.
func (s *stepRunner) enrich(_ context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	c.LLMSkipped = capture.SkippedDisabled

	return c, capture.OK()
}

// writeNote renders and creates the note (Create is idempotent through its
// deterministic memo id) and then attaches the audio (Attach dedupes). The
// note name is kept even when the attach fails, so a retry resumes there.
// MinimalNote keeps the text (truncated to the limit) and drops only the
// attachment; LLM output is not rendered yet.
func (s *stepRunner) writeNote(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	in := render.Input{Text: c.Text, Failure: c.Failure, InboxTag: s.cfg.InboxTag, TodoTag: s.cfg.TodoTag}
	if c.MinimalNote && c.AudioPath != "" && c.Failure == "" {
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
	name, link, out := s.d.Notes.Create(ctx, NoteInput{
		CaptureID:  c.ID,
		Content:    render.Render(in, s.cfg.NoteLimit),
		ReceivedAt: c.ReceivedAt,
	})
	if out.Class != capture.ClassOK {
		c.LastError = out.Reason

		return c, out
	}
	c.NoteName, c.NoteURL, c.TaskCount = name, link, 0

	if c.AudioPath == "" || c.MinimalNote {
		return c, capture.OK()
	}

	return s.attach(ctx, c)
}

// attach uploads the audio to the note. A missing file is not an error: the
// note exists and the audio cannot be recovered.
func (s *stepRunner) attach(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	data, err := s.d.Audio.Read(c.AudioPath)
	if errors.Is(err, fs.ErrNotExist) {
		s.log(c, "audio file missing, note saved without attachment", err)

		return c, capture.OK()
	}
	if err != nil {
		s.log(c, "audio read failed", err)

		return c, capture.Temporary("audio store: " + err.Error())
	}
	out := s.d.Notes.Attach(ctx, c.NoteName, s.attachmentName(c), audioMime(c.AudioPath), data)
	if out.Class != capture.ClassOK {
		// Policy.Next drops the reason of a permanent outcome; the minimal
		// rerun reads it back from LastError.
		c.LastError = out.Reason

		return c, out
	}

	return c, capture.OK()
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

// cleanup removes the audio and the content (FR-012); the identity needed
// for dedupe stays. Policy.Next clears SendNewReply when finalize_reply fails
// permanently and the step advances, so the new reply it asked for is
// delivered here, before anything is erased.
func (s *stepRunner) cleanup(ctx context.Context, c capture.Capture) (capture.Capture, capture.Outcome) {
	if c.SendNewReply {
		var out capture.Outcome
		if c, out = s.reply(ctx, c); out.Class != capture.ClassOK {
			return c, out
		}
	}
	if c.AudioPath != "" {
		if err := s.d.Audio.Remove(c.AudioPath); err != nil {
			s.log(c, "audio remove failed", err)

			return c, capture.Temporary("audio store: " + err.Error())
		}
	}
	c.Text, c.Language, c.EnrichedJSON, c.AudioPath, c.FileID = "", "", "", "", ""

	return c, capture.OK()
}

// replyText builds the final reply (contracts/telegram-ux.md), HTML-escaped.
func replyText(c capture.Capture) string {
	link := `<a href="` + html.EscapeString(c.NoteURL) + `">open</a>`
	if reason, ok := strings.CutPrefix(c.Failure, attachFailedMark); ok {
		return fmt.Sprintf("⚠ saved, audio not attached — %s · %s", html.EscapeString(reason), link)
	}
	if c.Failure != "" {
		return fmt.Sprintf("⚠ saved audio only — %s · %s", html.EscapeString(c.Failure), link)
	}
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

// audioName is "<id>.<ext>": Ogg unless the MIME type says otherwise.
func audioName(c capture.Capture) string {
	return strconv.FormatInt(c.ID, 10) + audioExt(c.MimeType)
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

// attachmentName is the deterministic Memos attachment filename
// voice-<received_at local date>.<ext>. It never carries the local file name,
// which holds the Telegram chat ID. Attach dedupes within the capture's own
// memo, so same-day captures cannot collide.
func (s *stepRunner) attachmentName(c capture.Capture) string {
	loc := s.cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	ext := path.Ext(c.AudioPath)
	if ext == "" {
		ext = extOgg
	}

	return "voice-" + c.ReceivedAt.In(loc).Format(time.DateOnly) + ext
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
