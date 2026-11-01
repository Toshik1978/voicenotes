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
