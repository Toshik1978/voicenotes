package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func clock() time.Time { return time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC) }

func healthy() Input {
	now := clock()

	return Input{
		Now:         now,
		Threshold:   30 * time.Minute,
		WorkerAlive: true,
		StoreOK:     true,
		LastPollOK:  now.Add(-time.Minute),
	}
}

func TestCompute(t *testing.T) {
	now := clock()
	tests := []struct {
		name   string
		mod    func(*Input)
		status Status
		code   int
	}{
		{"all fine", func(*Input) {}, StatusOK, 200},
		{
			"young backlog is ok",
			func(i *Input) { i.Pending = 2; i.OldestPendingAge = 10 * time.Minute; i.WaitingOn = "stt" },
			StatusOK,
			200,
		},
		{
			"backlog at threshold is ok",
			func(i *Input) { i.Pending = 1; i.OldestPendingAge = 30 * time.Minute },
			StatusOK,
			200,
		},
		{
			"old backlog degrades",
			func(i *Input) { i.Pending = 3; i.OldestPendingAge = 47 * time.Minute; i.WaitingOn = "stt" },
			StatusDegraded,
			200,
		},
		{
			"misconfigured head degrades at once",
			func(i *Input) {
				i.Pending = 1
				i.OldestPendingAge = time.Minute
				i.WaitingOn = "stt"
				i.Misconfig = true
			},
			StatusDegraded,
			200,
		},
		{
			"misconfig flag without a backlog is ignored",
			func(i *Input) { i.Misconfig = true },
			StatusOK,
			200,
		},
		{
			"unhealthy beats misconfig",
			func(i *Input) { i.Pending = 1; i.Misconfig = true; i.StoreOK = false },
			StatusUnhealthy,
			503,
		},
		{"stale poll degrades", func(i *Input) { i.LastPollOK = now.Add(-31 * time.Minute) }, StatusDegraded, 200},
		{
			"never polled, just started",
			func(i *Input) { i.LastPollOK = time.Time{}; i.Started = now.Add(-time.Minute) },
			StatusOK,
			200,
		},
		{
			"never polled, started long ago",
			func(i *Input) { i.LastPollOK = time.Time{}; i.Started = now.Add(-time.Hour) },
			StatusDegraded,
			200,
		},
		{"no poll and no start time", func(i *Input) { i.LastPollOK = time.Time{} }, StatusOK, 200},
		{"store down", func(i *Input) { i.StoreOK = false }, StatusUnhealthy, 503},
		{"worker stopped", func(i *Input) { i.WorkerAlive = false }, StatusUnhealthy, 503},
		{
			"unhealthy beats degraded",
			func(i *Input) { i.WorkerAlive = false; i.OldestPendingAge = time.Hour; i.Pending = 1 },
			StatusUnhealthy,
			503,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := healthy()
			tt.mod(&in)
			r := Compute(in)
			if r.Status != tt.status {
				t.Fatalf("status = %q, want %q", r.Status, tt.status)
			}
			if got := r.Status.HTTPCode(); got != tt.code {
				t.Fatalf("code = %d, want %d", got, tt.code)
			}
		})
	}
}

func TestComputeBody(t *testing.T) {
	in := healthy()
	in.Pending = 3
	in.OldestPendingAge = 47*time.Minute + 12*time.Second
	in.WaitingOn = "stt"
	in.LastPollOK = time.Date(2026, 10, 30, 2, 14, 5, 0, time.UTC)
	in.LastDigest = "2026-10-30"
	b, err := json.Marshal(Compute(in))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"status":"degraded","pending":3,"oldest_pending_age":"47m12s","waiting_on":"stt","last_poll_ok":"2026-10-30T02:14:05Z","last_digest":"2026-10-30"}`
	if string(b) != want {
		t.Fatalf("body = %s\nwant   %s", b, want)
	}
}

func TestComputeBodyOmitsUnknown(t *testing.T) {
	in := healthy()
	in.LastPollOK = time.Time{}
	b, err := json.Marshal(Compute(in))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"status":"ok","pending":0,"oldest_pending_age":"0s"}`
	if string(b) != want {
		t.Fatalf("body = %s\nwant   %s", b, want)
	}
}

type fakeSource struct{ in Input }

func (f fakeSource) Snapshot(context.Context) Input { return f.in }

func TestHandler(t *testing.T) {
	bad := healthy()
	bad.StoreOK = false
	tests := []struct {
		name   string
		method string
		in     Input
		code   int
		status Status
	}{
		{"ok", http.MethodGet, healthy(), 200, StatusOK},
		{"unhealthy", http.MethodGet, bad, 503, StatusUnhealthy},
		{"head allowed", http.MethodHead, healthy(), 200, ""},
		{"post rejected", http.MethodPost, healthy(), 405, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler(
				fakeSource{tt.in},
			).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, "/healthz", http.NoBody))
			if rec.Code != tt.code {
				t.Fatalf("code = %d, want %d", rec.Code, tt.code)
			}
			if tt.status == "" {
				return
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content-type = %q", ct)
			}
			var got Report
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.status {
				t.Fatalf("status = %q, want %q", got.Status, tt.status)
			}
		})
	}
}
