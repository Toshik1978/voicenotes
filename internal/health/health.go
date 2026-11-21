// Package health computes the service health status from a snapshot of its
// state and serves it on GET /healthz. The computation is pure; the caller
// supplies the snapshot through a Source.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Status is the overall health verdict.
type Status string

// The three verdicts defined by contracts/health.md.
const (
	StatusOK        Status = "ok"
	StatusDegraded  Status = "degraded"
	StatusUnhealthy Status = "unhealthy"
)

// HTTPCode maps a status to its HTTP code. Degraded is 200 on purpose so a
// failing dependency never makes the container get restarted.
func (s Status) HTTPCode() int {
	if s == StatusUnhealthy {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

// Input is a point-in-time snapshot of everything the status depends on.
type Input struct {
	Now       time.Time
	Started   time.Time     // process start; the poll reference until the first successful poll
	Threshold time.Duration // HEALTH_PENDING_THRESHOLD

	Pending          int           // unfinished captures
	OldestPendingAge time.Duration // age of the oldest unfinished capture
	WaitingOn        string        // what the oldest capture waits on (e.g. "stt"); empty if none
	Misconfig        bool          // the oldest capture waits on a misconfiguration (401, 403, 404)

	LastPollOK  time.Time // last successful Telegram poll; zero if none yet
	WorkerAlive bool
	StoreOK     bool
	LastDigest  string // date of the last digest sent (YYYY-MM-DD); empty if none

	RecorderAlerts []RecorderAlert // every known recorder; empty when the mic API is off
}

// RecorderAlert is one recorder and its outstanding conditions: Alert is
// "ok" (or empty) for none, else the condition names joined by commas, such
// as "missing_receiver,storage_full".
type RecorderAlert struct {
	Name     string
	Alert    string
	LastSeen time.Time
}

// outstanding reports whether the recorder has any condition raised.
func (r RecorderAlert) outstanding() bool { return r.Alert != "" && r.Alert != "ok" }

// RecorderReport is the JSON form of a RecorderAlert.
type RecorderReport struct {
	Name     string `json:"name"`
	Alert    string `json:"alert"`
	LastSeen string `json:"last_seen"`
}

// Report is the verdict plus the JSON body of GET /healthz.
type Report struct {
	Status           Status `json:"status"`
	Pending          int    `json:"pending"`
	OldestPendingAge string `json:"oldest_pending_age"`
	WaitingOn        string `json:"waiting_on,omitempty"`
	LastPollOK       string `json:"last_poll_ok,omitempty"`
	LastDigest       string `json:"last_digest,omitempty"`

	Recorders []RecorderReport `json:"recorders,omitempty"`
}

// Compute derives the report from the snapshot.
func Compute(in Input) Report {
	r := Report{
		Status:           StatusOK,
		Pending:          in.Pending,
		OldestPendingAge: in.OldestPendingAge.Round(time.Second).String(),
		WaitingOn:        in.WaitingOn,
		LastDigest:       in.LastDigest,
	}
	if !in.LastPollOK.IsZero() {
		r.LastPollOK = in.LastPollOK.UTC().Format(time.RFC3339)
	}

	degraded := pollStale(in)
	for _, rec := range in.RecorderAlerts {
		r.Recorders = append(r.Recorders, RecorderReport{
			Name: rec.Name, Alert: rec.Alert, LastSeen: rec.LastSeen.UTC().Format(time.RFC3339),
		})
		degraded = degraded || rec.outstanding()
	}

	switch {
	case !in.StoreOK || !in.WorkerAlive:
		r.Status = StatusUnhealthy
	case in.Pending > 0 && (in.Misconfig || in.OldestPendingAge > in.Threshold), degraded:
		r.Status = StatusDegraded
	}

	return r
}

// pollStale reports whether polling has failed for longer than the threshold.
// Before the first successful poll the process start time is the reference.
func pollStale(in Input) bool {
	ref := in.LastPollOK
	if ref.IsZero() {
		ref = in.Started
	}
	return !ref.IsZero() && in.Now.Sub(ref) > in.Threshold
}

// Source provides the current snapshot.
type Source interface {
	Snapshot(ctx context.Context) Input
}

// Handler serves GET /healthz.
func Handler(src Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rep := Compute(src.Snapshot(req.Context()))
		body, err := json.Marshal(rep)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.Status.HTTPCode())
		if req.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})
}
