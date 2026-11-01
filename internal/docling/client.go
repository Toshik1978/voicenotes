// Package docling is a thin client for docling-serve's async conversion API
// (contracts/external-apis.md §1, research R1). It turns one file into
// Markdown and classifies every result as a capture.Outcome tagged
// capture.WaitingDocling. It never logs, and outcome reasons never carry the
// API key, file content or response bodies.
package docling

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// Permanent reasons a file can fail with; they end up in the note.
const (
	// ReasonUnreadable is a conversion that ran and failed or was skipped.
	ReasonUnreadable = "document reader could not read it"
	// ReasonUnsupported is a submit rejected with 415.
	ReasonUnsupported = "document type not supported"
	// ReasonRejected is a submit rejected with 400, 413 or 422.
	ReasonRejected = "file rejected by document reader"
)

const (
	defaultHTTPTimeout = 120 * time.Second
	pollInterval       = 2 * time.Second
	maxResultBytes     = 32 << 20
	maxDiscardBytes    = 1 << 16
	reasonMalformed    = "malformed response"
	reasonTimedOut     = "timed out"
	reasonTaskLost     = "task lost"
)

// ConversionStatus values docling-serve uses for task_status and status.
const (
	statusPending        = "pending"
	statusStarted        = "started"
	statusSuccess        = "success"
	statusPartialSuccess = "partial_success"
	statusFailure        = "failure"
	statusSkipped        = "skipped"
)

// Config configures a Client.
type Config struct {
	// BaseURL is DOCLING_URL, for example http://docling.example.org:5001.
	BaseURL string
	// APIKey is optional; X-Api-Key is sent only when it is set.
	APIKey string
	// MaxPages bounds page_range to 1..MaxPages.
	MaxPages int
	// Timeout is DOCLING_TIMEOUT: the wait from submit to a final task
	// status, also sent as document_timeout.
	Timeout time.Duration
	// Version is used for "User-Agent: VoiceNotes/<version>".
	Version string
}

// Result is the outcome of one conversion. Markdown is meaningful only when
// the Outcome is OK and may be empty. TimedOut reports that no final task
// status arrived within Config.Timeout of the submit; the Outcome is then
// Temporary, and the caller applies its per-file timeout policy.
type Result struct {
	Markdown string
	TimedOut bool
}

// Client calls docling-serve's v1 async endpoints.
type Client struct {
	cfg         Config
	client      *http.Client
	interval    time.Duration
	resultLimit int64
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
}

// NewClient builds a Client. A nil client gets a default one with a 120s
// per-request timeout; the overall wait is bounded by Config.Timeout.
func NewClient(cfg Config, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}

	return &Client{
		cfg:         cfg,
		client:      client,
		interval:    pollInterval,
		resultLimit: maxResultBytes,
		now:         time.Now,
		sleep:       sleepCtx,
	}
}

// Convert submits one file, polls every 2s until the task finishes or
// Config.Timeout passes, and fetches document.md_content. Classification
// follows the contract table: HTTP via capture.ClassifyHTTP, transport errors
// via capture.NetworkError, a failed or skipped conversion is permanent, and
// malformed JSON is temporary.
func (c *Client) Convert(ctx context.Context, name, mime string, data []byte) (Result, capture.Outcome) {
	id, out := c.submit(ctx, name, mime, data)
	if out.Class != capture.ClassOK {
		return Result{}, out.On(capture.WaitingDocling)
	}
	timedOut, out := c.wait(ctx, id)
	if timedOut {
		return Result{TimedOut: true}, capture.Temporary(reasonTimedOut).On(capture.WaitingDocling)
	}
	if out.Class != capture.ClassOK {
		return Result{}, out.On(capture.WaitingDocling)
	}
	text, out := c.result(ctx, id)
	if out.Class != capture.ClassOK {
		return Result{}, out.On(capture.WaitingDocling)
	}

	return Result{Markdown: text}, capture.OK()
}

func (c *Client) submit(ctx context.Context, name, mime string, data []byte) (string, capture.Outcome) {
	body, contentType, err := c.buildForm(name, mime, data)
	if err != nil {
		return "", capture.Permanent("build request: " + err.Error())
	}
	var parsed struct {
		TaskID string `json:"task_id"`
	}
	status, out := c.do(ctx, http.MethodPost, "/v1/convert/file/async", body, contentType, maxDiscardBytes, &parsed)
	if out.Class == capture.ClassPermanent {
		return "", submitRejection(status)
	}
	if out.Class != capture.ClassOK {
		return "", out
	}
	if parsed.TaskID == "" {
		return "", capture.Temporary(reasonMalformed)
	}

	return parsed.TaskID, capture.OK()
}

// wait polls until the task reaches a final status. It reports a timeout
// when none arrives within Config.Timeout of the submit.
func (c *Client) wait(ctx context.Context, id string) (bool, capture.Outcome) {
	deadline := c.now().Add(c.cfg.Timeout)
	path := "/v1/status/poll/" + url.PathEscape(id)
	for {
		var parsed struct {
			TaskStatus string `json:"task_status"`
		}
		status, out := c.do(ctx, http.MethodGet, path, nil, "", maxDiscardBytes, &parsed)
		if out.Class != capture.ClassOK {
			return false, afterSubmit(status, out)
		}
		switch parsed.TaskStatus {
		case statusSuccess, statusPartialSuccess:
			return false, capture.OK()
		case statusFailure, statusSkipped:
			return false, capture.Permanent(ReasonUnreadable)
		case "":
			return false, capture.Temporary(reasonMalformed)
		case statusPending, statusStarted:
			// Not done yet: keep polling, as for a status this client does
			// not know.
		}
		left := deadline.Sub(c.now())
		if left <= 0 {
			return true, capture.OK()
		}
		if err := c.sleep(ctx, min(c.interval, left)); err != nil {
			return false, capture.NetworkError(err)
		}
	}
}

func (c *Client) result(ctx context.Context, id string) (string, capture.Outcome) {
	var parsed struct {
		Kind     string `json:"kind"`
		Status   string `json:"status"`
		Document struct {
			MDContent *string `json:"md_content"`
		} `json:"document"`
	}
	status, out := c.do(ctx, http.MethodGet, "/v1/result/"+url.PathEscape(id), nil, "", c.resultLimit, &parsed)
	if out.Class != capture.ClassOK {
		return "", afterSubmit(status, out)
	}
	if parsed.Kind == "TaskFailureResult" {
		return "", capture.Permanent(ReasonUnreadable)
	}
	switch parsed.Status {
	case statusSuccess, statusPartialSuccess:
		if parsed.Document.MDContent == nil {
			return "", capture.OK()
		}

		return *parsed.Document.MDContent, capture.OK()
	case statusFailure, statusSkipped:
		return "", capture.Permanent(ReasonUnreadable)
	default:
		return "", capture.Temporary(reasonMalformed)
	}
}

// do sends one request and decodes a 2xx JSON body of at most limit bytes
// into into. A body over the limit is malformed. It returns the HTTP status,
// or 0 when no response arrived.
func (c *Client) do(
	ctx context.Context, method, path string, body io.Reader, contentType string, limit int64, into any,
) (int, capture.Outcome) {
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, capture.Misconfig("invalid DOCLING_URL")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "VoiceNotes/"+c.cfg.Version)
	if c.cfg.APIKey != "" {
		req.Header.Set("X-Api-Key", c.cfg.APIKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, capture.NetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if out := capture.ClassifyHTTP(resp.StatusCode); out.Class != capture.ClassOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardBytes))
		return resp.StatusCode, out
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, capture.NetworkError(err)
	}
	if int64(len(raw)) > limit || json.Unmarshal(raw, into) != nil {
		return resp.StatusCode, capture.Temporary(reasonMalformed)
	}

	return resp.StatusCode, capture.OK()
}

// buildForm encodes the submit form pinned in client_test.go: page_range is
// the same field sent twice, first and last page.
func (c *Client) buildForm(name, mime string, data []byte) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if mime == "" {
		mime = "application/octet-stream"
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		`form-data; name="files"; filename="`+quoteEscaper.Replace(name)+`"`)
	h.Set("Content-Type", mime)
	fw, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := fw.Write(data); err != nil {
		return nil, "", fmt.Errorf("write file: %w", err)
	}
	fields := [][2]string{
		{"to_formats", "md"},
		{"image_export_mode", "placeholder"},
		{"include_images", "false"},
		{"do_ocr", "true"},
		{"abort_on_error", "false"},
		{"page_range", "1"},
		{"page_range", strconv.Itoa(c.cfg.MaxPages)},
		{"document_timeout", strconv.FormatFloat(c.cfg.Timeout.Seconds(), 'f', -1, 64)},
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return nil, "", fmt.Errorf("write field %s: %w", f[0], err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart: %w", err)
	}

	return &buf, mw.FormDataContentType(), nil
}

// quoteEscaper matches mime/multipart's escaping of quoted parameters.
var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`) //nolint:gochecknoglobals // immutable replacer

// submitRejection names why docling-serve refused the file (contract table).
func submitRejection(status int) capture.Outcome {
	if status == http.StatusUnsupportedMediaType {
		return capture.Permanent(ReasonUnsupported)
	}

	return capture.Permanent(ReasonRejected)
}

// afterSubmit reclassifies a poll or result failure. A successful submit
// proves the URL and key are right, so a 404 there means docling-serve lost
// the task (for example on a restart): temporary, and the retry resubmits.
func afterSubmit(status int, out capture.Outcome) capture.Outcome {
	if status == http.StatusNotFound {
		return capture.Temporary(reasonTaskLost)
	}

	return out
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for docling: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}
