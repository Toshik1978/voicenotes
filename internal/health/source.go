package health

import (
	"context"
	"time"
)

// Queue is the read side of the local store that health needs.
type Queue interface {
	// CountUnfinished returns how many captures are not done.
	CountUnfinished(ctx context.Context) (int, error)
	// OldestUnfinished describes the oldest unfinished capture; it returns
	// nil when the queue is empty.
	OldestUnfinished(ctx context.Context) (*Oldest, error)
	// LastDigestDate returns the local date of the last digest actually
	// sent (YYYY-MM-DD; empty and skipped days do not count), or "" when
	// there is none.
	LastDigestDate(ctx context.Context) (string, error)
}

// Oldest describes the oldest unfinished capture.
type Oldest struct {
	ReceivedAt time.Time
	WaitingOn  string // empty if it is not waiting on a dependency
	Misconfig  bool   // the latest outcome was a misconfiguration; never carries error text
}

// Worker reports the liveness of the pipeline loop.
type Worker interface {
	Alive() bool
	// StoreOK is false while the worker's last store operation failed.
	StoreOK() bool
}

// CollectorConfig wires a Collector.
type CollectorConfig struct {
	Queue     Queue
	Worker    Worker
	LastPoll  func() time.Time // last successful Telegram poll; zero if none yet
	Clock     func() time.Time
	Started   time.Time // process start
	Threshold time.Duration
}

// Collector is the Source built from the live components.
type Collector struct{ cfg CollectorConfig }

var _ Source = (*Collector)(nil)

// NewCollector returns a Collector. Started is passed on so that, until the
// first successful poll, Compute measures poll staleness from process start
// and a just-started process is not reported degraded.
func NewCollector(cfg CollectorConfig) *Collector { return &Collector{cfg: cfg} }

// Snapshot gathers the current state. A failing store query marks the store
// as not OK; a failing digest lookup only leaves LastDigest empty.
func (c *Collector) Snapshot(ctx context.Context) Input {
	now := c.cfg.Clock()
	in := Input{
		Now:         now,
		Started:     c.cfg.Started,
		Threshold:   c.cfg.Threshold,
		LastPollOK:  c.cfg.LastPoll(),
		WorkerAlive: c.cfg.Worker.Alive(),
		StoreOK:     c.cfg.Worker.StoreOK(),
	}

	n, err := c.cfg.Queue.CountUnfinished(ctx)
	if err != nil {
		in.StoreOK = false
		return in
	}
	in.Pending = n

	oldest, err := c.cfg.Queue.OldestUnfinished(ctx)
	if err != nil {
		in.StoreOK = false
		return in
	}
	if oldest != nil {
		in.OldestPendingAge = max(now.Sub(oldest.ReceivedAt), 0)
		in.WaitingOn = oldest.WaitingOn
		in.Misconfig = oldest.Misconfig
	}

	if date, err := c.cfg.Queue.LastDigestDate(ctx); err == nil {
		in.LastDigest = date
	}

	return in
}
