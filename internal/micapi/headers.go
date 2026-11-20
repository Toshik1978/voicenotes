package micapi

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/voicenotes/internal/mic"
)

// Response statuses (contracts/mic-http-api.md).
const (
	statusStored       = "stored"
	statusDuplicate    = "duplicate"
	statusUnavailable  = "unavailable"
	statusCorrupt      = "corrupt"
	statusAccepted     = "accepted"
	statusBadRequest   = "bad_request"
	statusUnauthorized = "unauthorized"
	statusTooLarge     = "too_large"
	statusUnsupported  = "unsupported"
)

// Upload request headers.
const (
	headerSHA256    = "X-Content-Sha256"
	headerStartedAt = "X-Recording-Started-At"
	headerEndReason = "X-Recording-End-Reason"
	headerRecorder  = "X-Recorder"
	contentTypeWAV  = "audio/wav"
)

// recorderName is the allowed shape of a recorder name.
var recorderName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// sha256Hex is a lowercase hex SHA-256.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// rejection is a request refused before any work: the HTTP code and the
// response body. A reason never carries a secret or a request value.
type rejection struct {
	code           int
	status, reason string
}

func (r *rejection) Error() string { return strconv.Itoa(r.code) + " " + r.status + ": " + r.reason }

func badRequest(reason string) *rejection {
	return &rejection{http.StatusBadRequest, statusBadRequest, reason}
}

// authorized reports whether the Authorization header carries the bearer
// token, comparing in constant time. An empty token authorizes nothing.
func authorized(header, token string) bool {
	if token == "" {
		return false
	}
	scheme, cred, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(cred), []byte(token)) == 1
}

// parseRecordingID parses a recording ID: a decimal int64 > 0.
func parseRecordingID(s string) (int64, *rejection) {
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, badRequest("recording ID must be a positive decimal integer")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("recording ID must be a positive decimal integer")
	}

	return id, nil
}

// validRecorderName reports whether name matches [A-Za-z0-9._-]{1,64}.
func validRecorderName(name string) bool { return recorderName.MatchString(name) }

// upload is the validated metadata of a recording upload.
type upload struct {
	length    int64
	sha256    string
	startedAt time.Time
	endReason string
	recorder  string
}

// parseUpload validates the upload headers of a body of contentLength bytes
// (-1 when unknown) against the size limit.
func parseUpload(h http.Header, contentLength, maxBytes int64) (upload, *rejection) {
	if mt, _, err := mime.ParseMediaType(h.Get("Content-Type")); err != nil || mt != contentTypeWAV {
		return upload{}, &rejection{
			http.StatusUnsupportedMediaType,
			statusUnsupported,
			"Content-Type must be audio/wav",
		}
	}
	if contentLength < 0 {
		return upload{}, badRequest("Content-Length is required")
	}
	if contentLength > maxBytes {
		return upload{}, &rejection{
			http.StatusRequestEntityTooLarge,
			statusTooLarge,
			"recording exceeds the upload limit",
		}
	}
	up := upload{length: contentLength, sha256: h.Get(headerSHA256)}
	if !sha256Hex.MatchString(up.sha256) {
		return upload{}, badRequest(headerSHA256 + " must be a lowercase hex SHA-256")
	}
	at, err := time.Parse(time.RFC3339Nano, h.Get(headerStartedAt))
	if err != nil {
		return upload{}, badRequest(headerStartedAt + " must be an RFC 3339 time")
	}
	up.startedAt = at.UTC()
	up.endReason = h.Get(headerEndReason)
	switch mic.EndReason(up.endReason) {
	case mic.EndTap, mic.EndMaxLength, mic.EndReceiverLost, mic.EndInterrupted:
	default:
		return upload{}, badRequest(headerEndReason + " is not a known end reason")
	}
	up.recorder = h.Get(headerRecorder)
	if !validRecorderName(up.recorder) {
		return upload{}, badRequest(headerRecorder + " is not a valid recorder name")
	}

	return up, nil
}

// decodeJSON decodes exactly one JSON value from body into v.
func decodeJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return err //nolint:wrapcheck // only the fact of failure is used
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}

	return nil
}

// parseStarted validates a start notice body.
func parseStarted(body []byte) (time.Time, string, *rejection) {
	var b struct {
		StartedAt *time.Time `json:"started_at"`
		Recorder  string     `json:"recorder"`
	}
	if err := decodeJSON(body, &b); err != nil {
		return time.Time{}, "", badRequest("body must be JSON with started_at and recorder")
	}
	if b.StartedAt == nil {
		return time.Time{}, "", badRequest("started_at is required")
	}
	if !validRecorderName(b.Recorder) {
		return time.Time{}, "", badRequest("recorder is not a valid recorder name")
	}

	return b.StartedAt.UTC(), b.Recorder, nil
}

// checkIn is the validated body of a recorder check-in.
type checkIn struct {
	receiverPresent bool
	storageFull     bool
}

// parseCheckIn validates a check-in body. pending and oldest_pending_at are
// checked for shape but not stored.
func parseCheckIn(body []byte) (checkIn, *rejection) {
	var b struct {
		ReceiverPresent *bool      `json:"receiver_present"`
		StorageFull     *bool      `json:"storage_full"`
		Pending         int64      `json:"pending"`
		OldestPendingAt *time.Time `json:"oldest_pending_at"`
	}
	if err := decodeJSON(body, &b); err != nil {
		return checkIn{}, badRequest("body must be a JSON check-in")
	}
	if b.ReceiverPresent == nil || b.StorageFull == nil {
		return checkIn{}, badRequest("receiver_present and storage_full are required")
	}
	if b.Pending < 0 {
		return checkIn{}, badRequest("pending must not be negative")
	}

	return checkIn{receiverPresent: *b.ReceiverPresent, storageFull: *b.StorageFull}, nil
}
