package recorder

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/health"
)

func TestComputeHealth(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	threshold := 30 * time.Minute
	healthy := Status{Running: true, ReceiverPresent: true, InputGrabbed: true}

	tests := []struct {
		name string
		s    Status
		want HealthReport
	}{
		{
			name: "all well",
			s:    healthy,
			want: HealthReport{Status: health.StatusOK, Receiver: "connected", InputGrabbed: true, Storage: "ok"},
		},
		{
			name: "loop stopped",
			s:    Status{ReceiverPresent: true, InputGrabbed: true},
			want: HealthReport{
				Status:       health.StatusUnhealthy,
				Receiver:     "connected",
				InputGrabbed: true,
				Storage:      "ok",
			},
		},
		{
			name: "loop stopped wins over degraded",
			s:    Status{StorageFull: true, Misconfigured: true},
			want: HealthReport{
				Status:        health.StatusUnhealthy,
				Receiver:      "missing",
				Storage:       "full",
				Misconfigured: true,
			},
		},
		{
			name: "receiver missing",
			s:    Status{Running: true},
			want: HealthReport{Status: health.StatusDegraded, Receiver: "missing", Storage: "ok"},
		},
		{
			name: "storage full",
			s:    Status{Running: true, ReceiverPresent: true, InputGrabbed: true, StorageFull: true},
			want: HealthReport{
				Status:       health.StatusDegraded,
				Receiver:     "connected",
				InputGrabbed: true,
				Storage:      "full",
			},
		},
		{
			name: "spool unreadable",
			s:    Status{Running: true, ReceiverPresent: true, InputGrabbed: true, SpoolError: true},
			want: HealthReport{
				Status:       health.StatusDegraded,
				Receiver:     "connected",
				InputGrabbed: true,
				Storage:      "error",
			},
		},
		{
			name: "a rejected recording",
			s:    Status{Running: true, ReceiverPresent: true, InputGrabbed: true, Rejected: 1},
			want: HealthReport{
				Status: health.StatusDegraded, Receiver: "connected", InputGrabbed: true, Storage: "ok", Rejected: 1,
			},
		},
		{
			name: "misconfigured (401)",
			s:    Status{Running: true, ReceiverPresent: true, InputGrabbed: true, Misconfigured: true},
			want: HealthReport{
				Status:        health.StatusDegraded,
				Receiver:      "connected",
				InputGrabbed:  true,
				Storage:       "ok",
				Misconfigured: true,
			},
		},
		{
			name: "audio could not start a recording",
			s:    Status{Running: true, ReceiverPresent: true, InputGrabbed: true, AudioError: true},
			want: HealthReport{
				Status:       health.StatusDegraded,
				Receiver:     "connected",
				InputGrabbed: true,
				Storage:      "ok",
				AudioError:   true,
			},
		},
		{
			name: "a recording could not be finished",
			s:    Status{Running: true, ReceiverPresent: true, InputGrabbed: true, FinishError: true},
			want: HealthReport{
				Status:       health.StatusDegraded,
				Receiver:     "connected",
				InputGrabbed: true,
				Storage:      "error",
			},
		},
		{
			name: "pending within the threshold",
			s: Status{
				Running: true, ReceiverPresent: true, InputGrabbed: true,
				Pending: 2, OldestPendingAt: now.Add(-threshold),
			},
			want: HealthReport{
				Status: health.StatusOK, Receiver: "connected", InputGrabbed: true, Storage: "ok",
				Pending: 2, OldestPendingAge: "30m0s",
			},
		},
		{
			name: "pending older than the threshold",
			s: Status{
				Running: true, ReceiverPresent: true, InputGrabbed: true,
				Pending: 1, OldestPendingAt: now.Add(-threshold - time.Second),
			},
			want: HealthReport{
				Status: health.StatusDegraded, Receiver: "connected", InputGrabbed: true, Storage: "ok",
				Pending: 1, OldestPendingAge: "30m1s",
			},
		},
		{
			name: "input not grabbed while the receiver is present degrades",
			s:    Status{Running: true, ReceiverPresent: true},
			want: HealthReport{Status: health.StatusDegraded, Receiver: "connected", Storage: "ok"},
		},
		{
			name: "recording",
			s:    Status{Running: true, ReceiverPresent: true, InputGrabbed: true, Recording: true},
			want: HealthReport{
				Status:       health.StatusOK,
				Receiver:     "connected",
				InputGrabbed: true,
				Storage:      "ok",
				Recording:    true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ComputeHealth(tt.s, now, threshold); got != tt.want {
				t.Errorf("ComputeHealth() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHealthHandler(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		s        Status
		wantCode int
		want     health.Status
	}{
		{"ok", Status{Running: true, ReceiverPresent: true, InputGrabbed: true}, http.StatusOK, health.StatusOK},
		{"degraded is 200", Status{Running: true}, http.StatusOK, health.StatusDegraded},
		{"unhealthy is 503", Status{}, http.StatusServiceUnavailable, health.StatusUnhealthy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := HealthHandler(func() Status { return tt.s }, func() time.Time { return now }, time.Minute)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", http.NoBody))

			if rec.Code != tt.wantCode {
				t.Errorf("code = %d, want %d", rec.Code, tt.wantCode)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if got["status"] != string(tt.want) {
				t.Errorf("status = %v, want %s", got["status"], tt.want)
			}
			for _, k := range []string{"receiver", "storage", "pending", "rejected"} {
				if _, ok := got[k]; !ok {
					t.Errorf("body %s lacks %q", rec.Body.String(), k)
				}
			}
		})
	}
}

func TestHealthHandlerRejectsOtherMethods(t *testing.T) {
	h := HealthHandler(func() Status { return Status{} }, time.Now, time.Minute)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/healthz", http.NoBody))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", rec.Code)
	}
}
