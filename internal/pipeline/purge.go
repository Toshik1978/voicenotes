package pipeline

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

const (
	// DoneRetention is how long finished captures stay for dedupe
	// (data-model "Purge").
	DoneRetention = 30 * 24 * time.Hour
	// PurgeInterval is how often done rows are purged (daily).
	PurgeInterval = 24 * time.Hour
)

// Purger deletes finished captures last updated before a cutoff.
type Purger interface {
	PurgeDone(ctx context.Context, olderThan time.Time) (int64, error)
}

// PurgeLoop purges done rows older than DoneRetention once at start and then
// every interval, until ctx is cancelled. A purge error is logged (without
// capture data) and never stops the loop. A nil log means no logging.
func PurgeLoop(ctx context.Context, p Purger, interval time.Duration, now func() time.Time, log *zap.Logger) {
	if log == nil {
		log = zap.NewNop()
	}
	purge := func() {
		n, err := p.PurgeDone(ctx, now().Add(-DoneRetention))
		switch {
		case errors.Is(err, context.Canceled):
		case err != nil:
			log.Error("purge failed", zap.Error(err))
		case n > 0:
			log.Info("purged finished captures", zap.Int64("count", n))
		}
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	purge()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}
