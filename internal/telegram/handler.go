// Package telegram is the Telegram adapter: it turns incoming messages into
// captures and sends the silent replies. The ingest logic in Handler works on
// the source-neutral Message and small interfaces (Replier, Downloader,
// Ingester, ReplyRecorder), so it is tested without a network; API binds
// those interfaces to telebot.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// Source is the capture source recorded for Telegram messages.
const Source = "telegram"

// MaxFileSize is the Telegram Bot API download limit (20 MB).
const MaxFileSize = 20 << 20

// Reply texts (contracts/telegram-ux.md).
const (
	TextPending     = "⏳"
	TextHelp        = "VoiceNotes: send a voice or text message to save it as a note."
	TextUnsupported = "VoiceNotes takes voice or text only."
	TextTooLarge    = "⚠ too large for Telegram bots (max 20 MB) — not saved."
	TextNotSaved    = "⚠ not saved — please send it again."
)

// ErrTooLarge is returned by a Downloader for a file over MaxFileSize. The
// capture is rejected with TextTooLarge rather than queued, because no retry
// can fetch it.
var ErrTooLarge = pipeline.ErrFileTooLarge

// Message is the source-neutral view of an incoming Telegram message.
type Message struct {
	ChatID   int64
	ID       int64
	SenderID int64
	Date     time.Time
	// Text is the message text; empty for media.
	Text string
	// Audio is set for voice and audio messages, forwarded or not.
	Audio *Audio
}

// Audio describes a voice or audio file held by Telegram.
type Audio struct {
	FileID   string
	MimeType string
	// Size is the size Telegram reported; 0 if unknown.
	Size int64
	// Voice is true for voice notes, false for audio files.
	Voice bool
}

// Replier sends and edits silent capture replies. Every call uses
// DisableNotification (FR-026). Both return the ID of the message that now
// holds the text: Edit falls back to a new silent reply when the message
// cannot be edited (deleted or too old), and then returns the new ID.
type Replier interface {
	Send(ctx context.Context, chatID, replyTo int64, text string) (int64, error)
	Edit(ctx context.Context, chatID, msgID int64, text string) (int64, error)
}

// Downloader fetches a Telegram file's bytes by file ID.
type Downloader interface {
	Download(ctx context.Context, fileID string) ([]byte, error)
}

// Ingester durably stores a capture, runs ack before the worker may touch
// it and then wakes the worker; it returns pipeline.ErrDuplicate for a
// redelivered message. *pipeline.Ingester implements it.
type Ingester interface {
	Ingest(ctx context.Context, c capture.Capture, ack pipeline.Ack) (capture.Capture, error)
}

// ReplyRecorder records the ⏳ reply of a stored capture without touching
// any other field, so it cannot race the worker.
type ReplyRecorder interface {
	SetReplyMessageID(ctx context.Context, id, msgID int64) error
}

// HandlerConfig wires a Handler. AllowedUsers may be empty (nobody is
// accepted); every other field except Log is required.
type HandlerConfig struct {
	AllowedUsers []int64
	// AudioDir is $DATA_DIR/audio; it is created if missing.
	AudioDir   string
	Replier    Replier
	Downloader Downloader
	Ingester   Ingester
	Replies    ReplyRecorder
	Log        *zap.Logger
}

// Handler is the ingest logic for one Telegram message. It is safe for
// concurrent use: telebot runs handlers concurrently.
type Handler struct {
	allowed  []int64
	audioDir string
	replier  Replier
	dl       Downloader
	ingester Ingester
	replies  ReplyRecorder
	log      *zap.Logger
}

// NewHandler validates cfg, creates the audio directory and returns a
// Handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	switch {
	case cfg.AudioDir == "":
		return nil, errors.New("telegram: empty audio dir")
	case cfg.Replier == nil:
		return nil, errors.New("telegram: nil replier")
	case cfg.Downloader == nil:
		return nil, errors.New("telegram: nil downloader")
	case cfg.Ingester == nil:
		return nil, errors.New("telegram: nil ingester")
	case cfg.Replies == nil:
		return nil, errors.New("telegram: nil reply recorder")
	}
	if err := os.MkdirAll(cfg.AudioDir, 0o700); err != nil {
		return nil, fmt.Errorf("telegram: create audio dir: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}

	return &Handler{
		allowed:  slices.Clone(cfg.AllowedUsers),
		audioDir: cfg.AudioDir,
		replier:  cfg.Replier,
		dl:       cfg.Downloader,
		ingester: cfg.Ingester,
		replies:  cfg.Replies,
		log:      log,
	}, nil
}

// Handle processes one message. Senders outside the allowlist are dropped
// without a reply (FR-002). It never returns an error: every failure is
// logged, and a capture that could be stored is never acknowledged before
// it is (Constitution VI).
func (h *Handler) Handle(ctx context.Context, m Message) {
	if !slices.Contains(h.allowed, m.SenderID) {
		h.log.Debug("ignoring message from a sender outside the allowlist",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID))

		return
	}

	switch {
	case m.Audio != nil:
		h.audio(ctx, m)
	case isHelp(m.Text):
		h.reply(ctx, m, TextHelp)
	case strings.TrimSpace(m.Text) != "":
		h.text(ctx, m)
	default:
		h.reply(ctx, m, TextUnsupported)
	}
}

func (h *Handler) text(ctx context.Context, m Message) {
	c := h.newCapture(m, capture.KindText, capture.StepEnrich)
	c.Text = m.Text
	h.ingest(ctx, m, c, h.ack(m))
}

// audio follows research R2: download, write and fsync the file, commit the
// row at transcribe, and only then send ⏳. If the file cannot be fetched or
// written, the row is committed at download with no reply; the worker's
// download step retries and sends ⏳ later.
func (h *Handler) audio(ctx context.Context, m Message) {
	a := m.Audio
	if a.Size > MaxFileSize {
		h.tooLarge(ctx, m)

		return
	}

	data, err := h.dl.Download(ctx, a.FileID)
	if errors.Is(err, ErrTooLarge) {
		h.tooLarge(ctx, m)

		return
	}
	if err != nil {
		h.log.Warn("audio download failed, queueing for the download step",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID), zap.Error(err))
		h.ingestUndownloaded(ctx, m)

		return
	}
	name, err := writeAudio(h.audioDir, audioPattern(m), data)
	if err != nil {
		h.log.Error("writing audio failed, queueing for the download step",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID), zap.Error(err))
		h.ingestUndownloaded(ctx, m)

		return
	}

	c := h.newAudioCapture(m, capture.StepTranscribe)
	c.AudioPath = name
	if c.FileSize == 0 {
		c.FileSize = int64(len(data))
	}
	if !h.ingest(ctx, m, c, h.ack(m)) {
		// Duplicate or store failure: the file belongs to no row.
		removeAudio(h.audioDir, name, h.log)
	}
}

// ingestUndownloaded commits an audio capture holding only the file ID.
// No reply is sent: the download step sends ⏳ once the audio is on disk.
func (h *Handler) ingestUndownloaded(ctx context.Context, m Message) {
	h.ingest(ctx, m, h.newAudioCapture(m, capture.StepDownload), nil)
}

func (h *Handler) newAudioCapture(m Message, start capture.Step) capture.Capture {
	c := h.newCapture(m, capture.KindAudio, start)
	c.FileID, c.MimeType, c.FileSize = m.Audio.FileID, m.Audio.MimeType, m.Audio.Size

	return c
}

func (h *Handler) newCapture(m Message, kind capture.Kind, start capture.Step) capture.Capture {
	c, err := capture.New(kind, start, m.Date)
	if err != nil {
		// Unreachable: kind and start are fixed pairs chosen above.
		panic(err)
	}
	c.Source, c.SourceChatID, c.SourceMessageID = Source, m.ChatID, m.ID

	return c
}

// ingest stores c, acknowledging it with ack (nil for none) before the
// worker may process it, and reports whether it was newly stored. A
// duplicate is ignored silently; any other failure gets the TextNotSaved
// reply.
func (h *Handler) ingest(ctx context.Context, m Message, c capture.Capture, ack pipeline.Ack) bool {
	stored, err := h.ingester.Ingest(ctx, c, ack)
	switch {
	case errors.Is(err, pipeline.ErrDuplicate):
		h.log.Info("duplicate delivery ignored", zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID))

		return false
	case err != nil:
		// Telegram will not redeliver the update, so the owner is asked to
		// resend rather than losing the capture silently.
		h.log.Error("storing capture failed; asking the sender to resend",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID), zap.Error(err))
		h.reply(ctx, m, TextNotSaved)

		return false
	}
	h.log.Info("capture stored",
		zap.Int64("capture", stored.ID), zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID),
		zap.Stringer("kind", stored.Kind), zap.Stringer("step", stored.Step))

	return true
}

// ack returns the acknowledgement for m: send ⏳ for the committed capture
// and record it. Ingest runs it before the worker may process the capture,
// so finalize_reply edits this ⏳ rather than sending a second reply. A
// failure here never undoes the row: finalize_reply falls back to a new
// message.
func (h *Handler) ack(m Message) pipeline.Ack {
	return func(ctx context.Context, stored capture.Capture) {
		msgID, err := h.replier.Send(ctx, m.ChatID, m.ID, TextPending)
		if err != nil {
			h.log.Warn("sending ⏳ failed", zap.Int64("capture", stored.ID), zap.Error(err))

			return
		}
		if err := h.replies.SetReplyMessageID(ctx, stored.ID, msgID); err != nil {
			h.log.Warn("recording the ⏳ reply failed",
				zap.Int64("capture", stored.ID), zap.Int64("reply", msgID), zap.Error(err))
		}
	}
}

func (h *Handler) tooLarge(ctx context.Context, m Message) {
	h.log.Info("rejecting audio over the Telegram download limit",
		zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID))
	h.reply(ctx, m, TextTooLarge)
}

func (h *Handler) reply(ctx context.Context, m Message, text string) {
	if _, err := h.replier.Send(ctx, m.ChatID, m.ID, text); err != nil {
		h.log.Warn("sending reply failed",
			zap.Int64("chat", m.ChatID), zap.Int64("message", m.ID), zap.Error(err))
	}
}

// isHelp reports whether text is /start or /help, with an optional
// @botname suffix and payload.
func isHelp(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	cmd, _, _ := strings.Cut(fields[0], "@")

	return cmd == "/start" || cmd == "/help"
}
