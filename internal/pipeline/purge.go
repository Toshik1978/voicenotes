package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

const (
	// DoneRetention is how long finished captures stay for dedupe
	// (data-model "Purge").
	DoneRetention = 30 * 24 * time.Hour
	// PurgeInterval is how often done rows are purged (daily).
	PurgeInterval = 24 * time.Hour
	// StartReplyInterval is how often stale start replies are checked, so a
	// message is edited within about StartReplyRetention plus this interval.
	StartReplyInterval = time.Hour
	// StartReplyRetention is how long a "recording..." message waits for its
	// upload before it is marked as not received.
	StartReplyRetention = 24 * time.Hour
	// RecorderRetention is how long a microphone recorder that stopped
	// checking in is remembered; after it the recorder is forgotten, so a
	// renamed or retired one no longer keeps health degraded.
	RecorderRetention = 30 * 24 * time.Hour

	textNotReceived = "⚠ recording not received"
)

// Purger deletes finished captures last updated before a cutoff.
type Purger interface {
	PurgeDone(ctx context.Context, olderThan time.Time) (int64, error)
}

// RecorderForgetter forgets the microphone recorders last seen before a
// cutoff and returns their names. A Purger that also implements it has its
// retired recorders forgotten by PurgeLoop.
type RecorderForgetter interface {
	ForgetRecorders(ctx context.Context, notSeenSince time.Time) ([]string, error)
}

// StartReply is a stored "recording..." message of a microphone recording.
type StartReply struct {
	RecordingID int64
	ChatID      int64
	MessageID   int64
}

// StartReplyStore lists and removes stored start replies. The production
// adapter wraps *sqlite.DB.
type StartReplyStore interface {
	StaleStartReplies(ctx context.Context, olderThan time.Time) ([]StartReply, error)
	TakeStartReply(ctx context.Context, recordingID int64) (chatID, messageID int64, ok bool, err error)
}

// ReplyPurger purges stale start replies. A Purger that also implements it
// is run by ReplyLoop.
type ReplyPurger interface {
	PurgeStartReplies(ctx context.Context, olderThan time.Time) (int, error)
}

// StartReplyJanitor is a ReplyPurger: it removes each stale start reply's
// row and edits the message to "recording not received".
type StartReplyJanitor struct {
	Store   StartReplyStore
	Replier Replier
	Log     *zap.Logger
}

var _ ReplyPurger = StartReplyJanitor{}

// PurgeStartReplies handles the replies created before olderThan and
// returns how many rows it removed. Each row is taken before its message is
// edited, and only a row this pass took is edited, so an upload that took it
// first keeps its own outcome in the chat. The edit is best-effort: an error
// is logged. A failure to list or remove stops the pass with the error.
func (j StartReplyJanitor) PurgeStartReplies(ctx context.Context, olderThan time.Time) (int, error) {
	log := j.Log
	if log == nil {
		log = zap.NewNop()
	}
	stale, err := j.Store.StaleStartReplies(ctx, olderThan)
	if err != nil {
		return 0, fmt.Errorf("pipeline: list stale start replies: %w", err)
	}
	removed := 0
	for _, r := range stale {
		if ctx.Err() != nil {
			return removed, fmt.Errorf("pipeline: purge of start replies stopped: %w", ctx.Err())
		}
		chatID, messageID, ok, err := j.Store.TakeStartReply(ctx, r.RecordingID)
		if err != nil {
			return removed, fmt.Errorf("pipeline: remove start reply of recording %d: %w", r.RecordingID, err)
		}
		if !ok {
			continue // taken by its upload or discard in the meantime
		}
		removed++
		if _, err = j.Replier.Edit(ctx, chatID, messageID, textNotReceived); err != nil {
			if ctx.Err() != nil {
				return removed, fmt.Errorf("pipeline: purge of start replies stopped: %w", ctx.Err())
			}
			log.Warn("marking start reply failed", zap.Int64("recording", r.RecordingID), zap.Error(err))
		}
	}

	return removed, nil
}

// HeldPurger purges held no-speech audio. A Purger that also implements it
// has its held audio purged by PurgeLoop; the implementation knows its hold.
type HeldPurger interface {
	PurgeHeld(ctx context.Context, now time.Time) (int, error)
}

// ExpiredHeld returns, in listing order, the regular files last modified
// more than hold before now: held no-speech audio whose hold has passed.
func ExpiredHeld(files []StoredFile, now time.Time, hold time.Duration) []string {
	var out []string
	cutoff := now.Add(-hold)
	for _, f := range files {
		if f.Regular && f.ModTime.Before(cutoff) {
			out = append(out, f.Name)
		}
	}

	return out
}

// HeldAudio is a HeldPurger over DATA_DIR/nospeech: it deletes the files
// whose mtime is older than Hold (MIC_NOSPEECH_HOLD). It touches nothing
// outside Dir and never recurses.
type HeldAudio struct {
	Dir  string
	Hold time.Duration
}

var _ HeldPurger = HeldAudio{}

// PurgeHeld deletes the expired files (see ExpiredHeld) and returns how many
// it removed. A missing directory holds nothing. A file that cannot be
// removed is skipped and reported in the error after the others are tried;
// one already gone counts as neither. Cancellation stops the pass.
func (h HeldAudio) PurgeHeld(ctx context.Context, now time.Time) (int, error) {
	files, err := listFiles(h.Dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, name := range ExpiredHeld(files, now, h.Hold) {
		if ctx.Err() != nil {
			return removed, fmt.Errorf("pipeline: purge of held audio stopped: %w", ctx.Err())
		}
		err := os.Remove(filepath.Join(h.Dir, name))
		switch {
		case err == nil:
			removed++
		case !errors.Is(err, fs.ErrNotExist):
			errs = append(errs, fmt.Errorf("pipeline: remove held audio: %w", err))
		}
	}

	return removed, errors.Join(errs...)
}

// PurgeLoop purges done rows older than DoneRetention once at start and then
// every interval (and, when p is a HeldPurger, the held no-speech audio past
// its hold; when p is a RecorderForgetter, the recorders not seen for
// RecorderRetention, logging each name), until ctx is cancelled. A purge error is logged (without
// capture data) and never stops the loop. A nil log means no logging.
func PurgeLoop(ctx context.Context, p Purger, interval time.Duration, now func() time.Time, log *zap.Logger) {
	if log == nil {
		log = zap.NewNop()
	}
	hp, hasHeld := p.(HeldPurger)
	rf, hasRecorders := p.(RecorderForgetter)
	run := func() {
		n, err := p.PurgeDone(ctx, now().Add(-DoneRetention))
		logPurge(log, "finished captures", n, err)
		if hasHeld {
			k, herr := hp.PurgeHeld(ctx, now())
			logPurge(log, "held no-speech audio", int64(k), herr)
		}
		if hasRecorders {
			names, ferr := rf.ForgetRecorders(ctx, now().Add(-RecorderRetention))
			for _, name := range names {
				log.Info("recorder forgotten", zap.String("recorder", name),
					zap.Duration("not_seen_for", RecorderRetention))
			}
			logPurge(log, "retired recorders", 0, ferr)
		}
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// logPurge reports one purge pass: a cancellation is silent, an error is
// logged without capture data and a non-empty pass is noted.
func logPurge(log *zap.Logger, what string, n int64, err error) {
	switch {
	case errors.Is(err, context.Canceled):
	case err != nil:
		log.Error("purge of "+what+" failed", zap.Error(err))
	case n > 0:
		log.Info("purged "+what, zap.Int64("count", n))
	}
}

// ReplyLoop marks the start replies older than StartReplyRetention as not
// received once at start and then every interval (StartReplyInterval in
// production), until ctx is cancelled. An error is logged and never stops the
// loop. A nil log means no logging.
func ReplyLoop(ctx context.Context, rp ReplyPurger, interval time.Duration, now func() time.Time, log *zap.Logger) {
	if log == nil {
		log = zap.NewNop()
	}
	run := func() {
		k, err := rp.PurgeStartReplies(ctx, now().Add(-StartReplyRetention))
		logPurge(log, "stale start replies", int64(k), err)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
