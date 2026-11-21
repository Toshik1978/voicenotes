package pipeline

import (
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// WakePending reports whether a Wake is queued and not yet consumed.
func (w *Worker) WakePending() bool { return len(w.wake) > 0 }

// HeldFor exposes heldFor: the worker checking c against the holds at now,
// as iterate does before running a due capture.
func (w *Worker) HeldFor(c capture.Capture, now time.Time) time.Duration { return w.heldFor(c, now) }

// SetSyncDir replaces the directory fsync Hold uses.
func (a *DirFiles) SetSyncDir(f func(dir string) error) { a.syncDir = f }

// SetNow replaces the clock Hold stamps a held file with.
func (a *DirFiles) SetNow(now func() time.Time) { a.now = now }
