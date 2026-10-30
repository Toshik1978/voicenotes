package capture

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	const maxIv = 5 * time.Minute
	tests := []struct {
		name     string
		attempts int
		rnd      float64
		want     time.Duration
	}{
		{"first, no jitter", 1, 0.5, 5 * time.Second},
		{"zero treated as first", 0, 0.5, 5 * time.Second},
		{"second doubles", 2, 0.5, 10 * time.Second},
		{"third", 3, 0.5, 20 * time.Second},
		{"low jitter -20%", 1, 0, 4 * time.Second},
		{"high jitter +20%", 1, 1, 6 * time.Second},
		{"capped", 10, 0.5, maxIv},
		{"capped even with high jitter", 10, 1, maxIv},
		{"huge attempts no overflow", 500, 0.5, maxIv},
		{"jitter below cap", 6, 0, 128 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Backoff(tt.attempts, maxIv, func() float64 { return tt.rnd })
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
	if got := Backoff(1, maxIv, nil); got != 5*time.Second {
		t.Errorf("nil rand = %v", got)
	}
	if got := Backoff(1, time.Second, nil); got != time.Second {
		t.Errorf("small cap = %v", got)
	}
}
