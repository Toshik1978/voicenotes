package capture

import "time"

const (
	backoffBase   = 5 * time.Second
	backoffJitter = 0.2
)

// Backoff returns the delay before retry number attempts (1-based): 5 s,
// doubling each time, with +-20% jitter, capped at maxInterval (research R11).
// rnd must return values in [0,1); nil disables jitter.
func Backoff(attempts int, maxInterval time.Duration, rnd func() float64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := backoffBase
	for i := 1; i < attempts && d < maxInterval; i++ {
		d *= 2
	}
	if rnd != nil {
		d = time.Duration(float64(d) * (1 + (rnd()*2-1)*backoffJitter))
	}
	if d > maxInterval {
		d = maxInterval
	}

	return d
}
