package recorder

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Toshik1978/voicenotes/internal/health"
)

// HealthReport is the verdict plus the JSON body of the recorder's GET /healthz.
type HealthReport struct {
	Status health.Status `json:"status"`
	// Receiver is "connected" or "missing".
	Receiver string `json:"receiver"`
	// InputGrabbed is false when the receiver's key is not held exclusively,
	// so a tap may also change the host volume. It is reported, not degraded.
	InputGrabbed bool `json:"input_grabbed"`
	Recording    bool `json:"recording"`
	// Storage is "ok", "full" (below MIC_SPOOL_MIN_FREE) or "error" (the
	// spool cannot be listed).
	Storage          string `json:"storage"`
	Pending          int    `json:"pending"`
	OldestPendingAge string `json:"oldest_pending_age,omitempty"`
	Rejected         int    `json:"rejected"`
	Misconfigured    bool   `json:"misconfigured,omitempty"`
}

// ComputeHealth derives the recorder's health from a status snapshot:
// unhealthy once the loops have stopped; degraded while the receiver is
// missing, storage is full or unreadable, a recording was rejected, the
// service refused the token (401), or a pending recording is older than
// threshold (MIC_PENDING_THRESHOLD); ok otherwise.
func ComputeHealth(s Status, now time.Time, threshold time.Duration) HealthReport {
	rep := HealthReport{
		Status:        health.StatusOK,
		Receiver:      "connected",
		InputGrabbed:  s.InputGrabbed,
		Recording:     s.Recording,
		Storage:       "ok",
		Pending:       s.Pending,
		Rejected:      s.Rejected,
		Misconfigured: s.Misconfigured,
	}

	degraded := s.Rejected > 0 || s.Misconfigured

	if !s.ReceiverPresent {
		rep.Receiver = "missing"
		degraded = true
	}

	switch {
	case s.StorageFull:
		rep.Storage = "full"
		degraded = true
	case s.SpoolError:
		rep.Storage = "error"
		degraded = true
	}

	if !s.OldestPendingAt.IsZero() {
		age := now.Sub(s.OldestPendingAt)
		rep.OldestPendingAge = age.Round(time.Second).String()
		degraded = degraded || age > threshold
	}

	switch {
	case !s.Running:
		rep.Status = health.StatusUnhealthy
	case degraded:
		rep.Status = health.StatusDegraded
	}

	return rep
}

// HealthHandler serves GET /healthz from the snapshot src returns: 503 when
// unhealthy, 200 otherwise (degraded included), with the report as JSON.
func HealthHandler(src func() Status, now func() time.Time, threshold time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		rep := ComputeHealth(src(), now(), threshold)

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
