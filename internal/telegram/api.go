package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	tele "gopkg.in/telebot.v4"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// Defaults for APIConfig.
const (
	defaultPollTimeout = 25 * time.Second
	defaultRetryBase   = time.Second
	maxRetry           = time.Minute
	// httpTimeout bounds every Bot API call, including a long poll.
	httpTimeout = time.Minute
	// handlerTimeout bounds one message's handling: a download (at most
	// httpTimeout), the file write, the insert and the ⏳ reply.
	handlerTimeout = 3 * time.Minute
)

// APIConfig configures the telebot binding.
type APIConfig struct {
	// Token is the bot token; it never appears in errors or logs.
	Token string
	// URL overrides the Bot API base URL (tests); empty means Telegram.
	URL string
	// PollTimeout is the long-poll timeout (default 25s).
	PollTimeout time.Duration
	// RetryBase is the first delay after a failed poll; it doubles up to a
	// minute (default 1s).
	RetryBase time.Duration
	Log       *zap.Logger
}

// MessageHandler processes one incoming message; *Handler implements it.
type MessageHandler interface {
	Handle(ctx context.Context, m Message)
}

// API binds Replier and Downloader to the Telegram Bot API through telebot
// and runs the long-polling loop.
type API struct {
	bot    *tele.Bot
	poller *poller
	log    *zap.Logger
}

var (
	_ Replier    = (*API)(nil)
	_ Downloader = (*API)(nil)
)

// NewAPI builds the telebot client. It makes no network call (no getMe), so
// the service starts even while Telegram is unreachable.
func NewAPI(cfg APIConfig) (*API, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram: empty bot token")
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	p := &poller{
		timeout:   cmpOr(cfg.PollTimeout, defaultPollTimeout),
		retryBase: cmpOr(cfg.RetryBase, defaultRetryBase),
		log:       log,
	}
	bot, err := tele.NewBot(tele.Settings{
		Token:   cfg.Token,
		URL:     cfg.URL,
		Poller:  p,
		Offline: true,
		// Run spawns and tracks the handler goroutines itself.
		Synchronous: true,
		Client:      &http.Client{Timeout: httpTimeout},
		OnError: func(err error, _ tele.Context) {
			log.Error("telegram handler error", zap.Error(err))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("telegram: create bot: %w", err)
	}

	return &API{bot: bot, poller: p, log: log}, nil
}

// Run long-polls for messages (allowed_updates=["message"]) and passes each
// to h until ctx is cancelled. Each message is handled in its own goroutine.
// Handlers get a context that shutdown does not cancel, bounded by
// handlerTimeout, and Run returns only after every handler has finished, so
// an ingest in flight at shutdown can still commit its row (Constitution VI).
// Run must be called at most once.
func (a *API) Run(ctx context.Context, h MessageHandler) {
	base := context.WithoutCancel(ctx)
	var wg sync.WaitGroup
	// telebot runs this synchronously in its update loop (Synchronous), so
	// wg.Add happens before Start returns and the Wait below cannot miss it.
	handle := func(c tele.Context) error {
		m := c.Message()
		if m == nil {
			return nil
		}
		msg := fromTele(m)
		wg.Go(func() {
			hctx, cancel := context.WithTimeout(base, handlerTimeout)
			defer cancel()
			h.Handle(hctx, msg)
		})

		return nil
	}
	// Text (commands included), voice and audio are captures or commands;
	// every other user content gets the "voice or text only" reply.
	for _, ep := range []string{
		tele.OnText, tele.OnVoice, tele.OnAudio, tele.OnMedia, tele.OnContact,
		tele.OnLocation, tele.OnVenue, tele.OnDice, tele.OnGame, tele.OnPoll,
	} {
		a.bot.Handle(ep, handle)
	}

	done := make(chan struct{})
	go func() {
		a.bot.Start()
		close(done)
	}()
	<-ctx.Done()
	a.bot.Stop()
	<-done
	wg.Wait()
}

// LastPoll is the time of the last successful getUpdates call; zero before
// the first.
func (a *API) LastPoll() time.Time { return a.poller.lastPoll() }

// Send sends a silent HTML reply to replyTo (0 for none). A deleted replyTo
// does not fail the send.
func (a *API) Send(ctx context.Context, chatID, replyTo int64, text string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("telegram: send: %w", err)
	}
	opts := silent()
	if replyTo != 0 {
		opts.ReplyTo = &tele.Message{ID: int(replyTo)}
		opts.AllowWithoutReply = true
	}
	m, err := a.bot.Send(tele.ChatID(chatID), text, opts)
	if err != nil {
		return 0, wrapAPI("send", err)
	}

	return int64(m.ID), nil
}

// SendWithSound sends text to chatID in HTML mode WITH a notification sound.
// It is for the morning digest only; every other bot message is silent.
func (a *API) SendWithSound(ctx context.Context, chatID int64, text string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("telegram: send: %w", err)
	}
	if _, err := a.bot.Send(tele.ChatID(chatID), text, &tele.SendOptions{ParseMode: tele.ModeHTML}); err != nil {
		return wrapAPI("send", err)
	}

	return nil
}

// Edit replaces the text of msgID silently. An unchanged text counts as
// success. If Telegram refuses the edit (the message was deleted or is too
// old), a new silent reply with the same text is sent and its ID returned.
// Transport errors, flood limits and server errors are returned for a later
// retry.
func (a *API) Edit(ctx context.Context, chatID, msgID int64, text string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("telegram: edit: %w", err)
	}
	sig := tele.StoredMessage{MessageID: strconv.FormatInt(msgID, 10), ChatID: chatID}
	_, err := a.bot.Edit(sig, text, silent())
	switch {
	case err == nil, notModified(err):
		return msgID, nil
	case editImpossible(err):
		a.log.Info("reply cannot be edited, sending a new one",
			zap.Int64("chat", chatID), zap.Int64("reply", msgID), zap.Error(err))

		return a.Send(ctx, chatID, msgID, text)
	}

	return 0, wrapAPI("edit", err)
}

// Download fetches a file's bytes. A file over MaxFileSize, whether
// Telegram refuses it or it turns out larger while reading, yields
// ErrTooLarge.
func (a *API) Download(ctx context.Context, fileID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("telegram: download: %w", err)
	}
	rc, err := a.bot.File(&tele.File{FileID: fileID})
	if err != nil {
		if errors.Is(err, tele.ErrTooLarge) || strings.Contains(err.Error(), "file is too big") {
			return nil, ErrTooLarge
		}

		return nil, wrapAPI("download", err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(io.LimitReader(rc, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("telegram: download: %w", err)
	}
	if len(data) > MaxFileSize {
		return nil, ErrTooLarge
	}

	return data, nil
}

func silent() *tele.SendOptions {
	return &tele.SendOptions{DisableNotification: true, ParseMode: tele.ModeHTML}
}

func notModified(err error) bool {
	return errors.Is(err, tele.ErrMessageNotModified) || errors.Is(err, tele.ErrSameMessageContent) ||
		strings.Contains(err.Error(), "message is not modified")
}

// editImpossible reports a Bad Request refusal of the edit itself, such as
// "message to edit not found" or "message can't be edited". telebot returns
// descriptions it does not know as plain errors, hence the text match.
func editImpossible(err error) bool {
	if te, ok := errors.AsType[*tele.Error](err); ok {
		return te.Code == http.StatusBadRequest
	}

	return strings.Contains(err.Error(), "Bad Request:")
}

// fromTele maps a telebot message to the source-neutral Message.
func fromTele(m *tele.Message) Message {
	out := Message{ID: int64(m.ID), Date: m.Time(), Text: m.Text}
	if m.Chat != nil {
		out.ChatID = m.Chat.ID
	}
	if m.Sender != nil {
		out.SenderID = m.Sender.ID
	}
	switch {
	case m.Voice != nil:
		out.Audio = &Audio{FileID: m.Voice.FileID, MimeType: m.Voice.MIME, Size: m.Voice.FileSize, Voice: true}
	case m.Audio != nil:
		out.Audio = &Audio{FileID: m.Audio.FileID, MimeType: m.Audio.MIME, Size: m.Audio.FileSize}
	default:
		out.Audio, out.Files = mapMedia(m)
	}
	if out.Files != nil {
		out.Files.Caption = m.Caption
		out.Files.AlbumID = m.AlbumID
	}

	return out
}

func cmpOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}

	return def
}

// poller long-polls getUpdates for messages only. Unlike tele.LongPoller it
// backs off after a failed poll instead of retrying in a tight loop, and it
// records the time of the last successful poll for health.
type poller struct {
	timeout   time.Duration
	retryBase time.Duration
	log       *zap.Logger
	last      atomic.Int64 // UnixNano of the last successful poll
	offset    int
}

// Poll implements tele.Poller.
func (p *poller) Poll(b *tele.Bot, dest chan tele.Update, stop chan struct{}) {
	failures := 0
	for {
		select {
		case <-stop:
			return
		default:
		}

		updates, err := p.fetch(b)
		if err != nil {
			failures++
			d := min(p.retryBase<<min(failures-1, 16), maxRetry)
			p.log.Warn("telegram poll failed", zap.Int("failures", failures), zap.Duration("retry_in", d),
				zap.Error(err))
			select {
			case <-stop:
				return
			case <-time.After(d):
			}

			continue
		}
		failures = 0
		p.last.Store(time.Now().UnixNano())
		for i := range updates {
			p.offset = updates[i].ID + 1
			select {
			case dest <- updates[i]:
			case <-stop:
				return
			}
		}
	}
}

func (p *poller) lastPoll() time.Time {
	if n := p.last.Load(); n != 0 {
		return time.Unix(0, n)
	}

	return time.Time{}
}

func (p *poller) fetch(b *tele.Bot) ([]tele.Update, error) {
	data, err := b.Raw("getUpdates", map[string]any{
		"offset":          p.offset,
		"timeout":         int(p.timeout / time.Second),
		"allowed_updates": []string{"message"},
	})
	if err != nil {
		return nil, fmt.Errorf("telegram: get updates: %w", err)
	}
	var resp struct {
		Result []tele.Update `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("telegram: decode updates: %w", err)
	}

	return resp.Result, nil
}

// wrapAPI wraps a Bot API failure for op. A refusal that retrying cannot fix
// (400 Bad Request, 403 Forbidden) also wraps pipeline.ErrRejected, so the
// pipeline gives up instead of blocking the queue. 429, 5xx and transport
// errors stay plain and are retried.
func wrapAPI(op string, err error) error {
	if te, ok := errors.AsType[*tele.Error](err); ok &&
		(te.Code == http.StatusBadRequest || te.Code == http.StatusForbidden) {
		return fmt.Errorf("telegram: %s: %w: %w", op, pipeline.ErrRejected, err)
	}

	return fmt.Errorf("telegram: %s: %w", op, err)
}

// mapMedia maps a photo, document, animation or video message. An audio
// document becomes an Audio, like a voice or audio message; everything else
// becomes a FileItem. Both results are nil for other messages.
func mapMedia(m *tele.Message) (*Audio, *FileItem) {
	switch {
	case m.Photo != nil:
		// The Bot API delivers the largest size, as JPEG.
		return nil, &FileItem{
			FileID: m.Photo.FileID,
			MIME:   "image/jpeg",
			Size:   m.Photo.FileSize,
			Kind:   capture.FileImage,
		}
	case m.Animation != nil:
		// Telegram also sets Document for an animation; the animation wins.
		a := m.Animation

		return nil, &FileItem{
			FileID: a.FileID,
			MIME:   a.MIME,
			Name:   a.FileName,
			Size:   a.FileSize,
			Kind:   capture.FileVideo,
		}
	case m.Document != nil:
		d := m.Document
		switch {
		case strings.HasPrefix(d.MIME, "audio/"):
			return &Audio{FileID: d.FileID, MimeType: d.MIME, Size: d.FileSize}, nil
		case strings.HasPrefix(d.MIME, "image/"):
			return nil, &FileItem{
				FileID: d.FileID,
				MIME:   d.MIME,
				Name:   d.FileName,
				Size:   d.FileSize,
				Kind:   capture.FileImage,
			}
		}

		return nil, &FileItem{
			FileID: d.FileID,
			MIME:   d.MIME,
			Name:   d.FileName,
			Size:   d.FileSize,
			Kind:   capture.FileDocument,
		}
	case m.Video != nil:
		v := m.Video

		return nil, &FileItem{
			FileID: v.FileID,
			MIME:   v.MIME,
			Name:   v.FileName,
			Size:   v.FileSize,
			Kind:   capture.FileVideo,
		}
	}

	return nil, nil
}
