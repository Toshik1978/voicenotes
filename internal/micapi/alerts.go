package micapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// AlertInterval is how often the alert loop evaluates the recorders.
const AlertInterval = 30 * time.Second

// AlertStore reads recorder check-ins and stores the outstanding alert of a
// recorder. It speaks mic types so micapi never imports the store; a thin
// adapter over *sqlite.DB is the production implementation.
type AlertStore interface {
	RecorderStatuses(ctx context.Context) ([]mic.RecorderStatus, error)
	SetRecorderAlert(ctx context.Context, name string, alert mic.Alert) error
}

// AlertLoop periodically evaluates every recorder and sends a silent
// Telegram message for each change of its alert.
//
// Ordering: the message is sent first and the new alert stored after it. A
// crash between the two repeats a message on the next pass; it never loses
// one. A failed send stores nothing, so the next pass retries.
type AlertLoop struct {
	store   AlertStore
	replier pipeline.Replier
	chat    int64
	silence time.Duration
	alerts  mic.Alerts
	now     func() time.Time
}

// NewAlertLoop builds the loop. chat is the notify chat, silence the
// MIC_SILENCE_ALERT threshold and loc the zone of the time in the silence
// message (nil means UTC).
func NewAlertLoop(
	store AlertStore, replier pipeline.Replier, chat int64, silence time.Duration, loc *time.Location,
) *AlertLoop {
	return &AlertLoop{
		store:   store,
		replier: replier,
		chat:    chat,
		silence: silence,
		alerts:  mic.Alerts{Location: loc},
		now:     time.Now,
	}
}

// SetClock overrides the clock (for tests).
func (l *AlertLoop) SetClock(now func() time.Time) { l.now = now }

// Run evaluates once at start and then every [AlertInterval] until ctx is
// cancelled. A failed pass is reported to onErr (if set) and never stops the
// loop.
func (l *AlertLoop) Run(ctx context.Context, onErr func(error)) {
	t := time.NewTicker(AlertInterval)
	defer t.Stop()
	l.runWith(ctx, t.C, onErr)
}

// RunWith is Run driven by the given tick channel (for tests).
func (l *AlertLoop) RunWith(ctx context.Context, ticks <-chan time.Time) {
	l.runWith(ctx, ticks, nil)
}

// Pass evaluates every recorder once. A failure for one recorder does not
// stop the others; the errors are joined.
func (l *AlertLoop) Pass(ctx context.Context) error {
	rows, err := l.store.RecorderStatuses(ctx)
	if err != nil {
		return fmt.Errorf("micapi: list recorders: %w", err)
	}
	now := l.now()
	var errs []error
	for _, r := range rows {
		// A recorder alerts only after its first check-in (FR-227a).
		if r.LastSeen.IsZero() {
			continue
		}
		if err := l.evaluate(ctx, r, now); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (l *AlertLoop) runWith(ctx context.Context, ticks <-chan time.Time, onErr func(error)) {
	pass := func() {
		if err := l.Pass(ctx); err != nil && onErr != nil && ctx.Err() == nil {
			onErr(err)
		}
	}
	pass()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			pass()
		}
	}
}

func (l *AlertLoop) evaluate(ctx context.Context, r mic.RecorderStatus, now time.Time) error {
	want, msg := l.alerts.Eval(r, now, l.silence)
	if msg == "" {
		return nil
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if _, err := l.replier.Send(sendCtx, l.chat, 0, msg); err != nil {
		return fmt.Errorf("micapi: send alert of recorder %q: %w", r.Name, err)
	}
	if err := l.store.SetRecorderAlert(ctx, r.Name, want); err != nil {
		return fmt.Errorf("micapi: store alert of recorder %q: %w", r.Name, err)
	}

	return nil
}
