package capture

import "time"

// Due is the result of asking the queue for the next capture to process.
// Capture is meaningful only when OK is true; otherwise WakeAt is when the
// lane's earliest capture becomes due, or the zero time when the lane is empty.
type Due struct {
	Capture Capture
	OK      bool
	WakeAt  time.Time
}
