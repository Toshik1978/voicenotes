package recorder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// noticeTimeout bounds a start or discard notice (contract: 5 s, no retries).
	noticeTimeout = 5 * time.Second
	// checkInTimeout bounds one check-in; the next interval retries.
	checkInTimeout = 10 * time.Second
	// maxErrBody caps how much of an error response is read for its reason.
	maxErrBody = 4 << 10

	statusCorrupt = "corrupt"
)

// Client is the recorder's HTTP side: it implements [Handoff], [Notifier]
// and [CheckIn] against the service's /mic/v1 API.
type Client struct {
	hc       *http.Client
	base     string
	token    string
	recorder string
}

var (
	_ Handoff  = (*Client)(nil)
	_ Notifier = (*Client)(nil)
	_ CheckIn  = (*Client)(nil)
)

// NewHTTPClient returns an *http.Client without an overall timeout (an
// upload of tens of megabytes may be slow; callers bound it with a context)
// but with transport-level timeouts for connecting and for the response.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}}
}

// NewClient returns a Client for the service at baseURL (MIC_SERVER_URL; a
// path prefix and a trailing slash are fine). A nil hc means
// [NewHTTPClient].
func NewClient(baseURL, token, recorder string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("recorder: MIC_SERVER_URL is not an http(s) URL")
	}

	if token == "" || recorder == "" {
		return nil, errors.New("recorder: upload token and recorder name are required")
	}

	if hc == nil {
		hc = NewHTTPClient()
	}

	u.RawQuery, u.Fragment = "", ""

	return &Client{hc: hc, base: strings.TrimRight(u.String(), "/"), token: token, recorder: recorder}, nil
}

// reply is a response body's JSON.
type reply struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func readReply(resp *http.Response) reply {
	var r reply

	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	_ = json.Unmarshal(b, &r)

	return r
}

func statusError(op string, code int, r reply) error {
	return fmt.Errorf("recorder: %s: HTTP %d status=%q reason=%q", op, code, r.Status, r.Reason)
}

// Upload implements [Handoff]: it hashes body, rewinds it and streams it
// as a PUT.
func (c *Client) Upload(ctx context.Context, rec Recording, body io.ReadSeeker, size int64) (Result, error) {
	sum, err := hashBody(body, size)
	if err != nil {
		return Retry, err
	}

	req, err := c.newRequest(ctx, http.MethodPut, c.recordingURL(rec.ID), io.NopCloser(io.LimitReader(body, size)))
	if err != nil {
		return Rejected, err
	}

	req.ContentLength = size
	req.Header.Set("Content-Type", "audio/wav")
	req.Header.Set("X-Content-Sha256", sum)
	req.Header.Set("X-Recording-Started-At", rec.StartedAt.UTC().Format(time.RFC3339Nano))
	req.Header.Set("X-Recording-End-Reason", string(rec.EndReason))
	req.Header.Set("X-Recorder", rec.Recorder)

	resp, err := c.do(req)
	if err != nil {
		return Retry, err
	}

	defer func() { _ = resp.Body.Close() }()

	return classify(resp)
}

// classify maps an upload response to a [Result] (contract "Client retry
// policy").
func classify(resp *http.Response) (Result, error) {
	switch code := resp.StatusCode; code {
	case http.StatusCreated:
		return Stored, nil
	case http.StatusOK:
		return Duplicate, nil
	case http.StatusUnauthorized:
		return Misconfigured, statusError("upload", code, readReply(resp))
	case http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
		return Rejected, statusError("upload", code, readReply(resp))
	case http.StatusBadRequest:
		r := readReply(resp)
		if r.Status == statusCorrupt {
			return Retry, statusError("upload", code, r)
		}

		return Rejected, statusError("upload", code, r)
	default: // 5xx and anything unforeseen: try again later
		return Retry, statusError("upload", code, readReply(resp))
	}
}

func hashBody(body io.ReadSeeker, size int64) (string, error) {
	h := sha256.New()

	n, err := io.Copy(h, io.LimitReader(body, size))
	if err != nil {
		return "", fmt.Errorf("recorder: hash recording: %w", err)
	}

	if n != size {
		return "", fmt.Errorf("recorder: recording is %d bytes, expected %d", n, size)
	}

	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("recorder: rewind recording: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// Started implements [Notifier]: one POST, 5 s, no retry.
func (c *Client) Started(ctx context.Context, id int64, startedAt time.Time) error {
	payload, err := json.Marshal(struct {
		StartedAt string `json:"started_at"`
		Recorder  string `json:"recorder"`
	}{startedAt.UTC().Format(time.RFC3339Nano), c.recorder})
	if err != nil {
		return fmt.Errorf("recorder: encode start notice: %w", err)
	}

	return c.simple(ctx, "start notice", http.MethodPost, c.recordingURL(id)+"/started",
		noticeTimeout, payload, http.StatusAccepted)
}

// Discarded implements [Notifier]: one DELETE, 5 s, no retry.
func (c *Client) Discarded(ctx context.Context, id int64) error {
	return c.simple(ctx, "discard notice", http.MethodDelete, c.recordingURL(id)+"/started",
		noticeTimeout, nil, http.StatusNoContent)
}

// Send implements [CheckIn].
func (c *Client) Send(ctx context.Context, s Status) error {
	var oldest *string

	if !s.OldestPendingAt.IsZero() {
		v := s.OldestPendingAt.UTC().Format(time.RFC3339Nano)
		oldest = &v
	}

	payload, err := json.Marshal(struct {
		OldestPendingAt *string `json:"oldest_pending_at"`
		Pending         int     `json:"pending"`
		ReceiverPresent bool    `json:"receiver_present"`
		StorageFull     bool    `json:"storage_full"`
	}{oldest, s.Pending, s.ReceiverPresent, s.StorageFull})
	if err != nil {
		return fmt.Errorf("recorder: encode check-in: %w", err)
	}

	return c.simple(ctx, "check-in", http.MethodPut, c.base+"/mic/v1/recorders/"+url.PathEscape(c.recorder),
		checkInTimeout, payload, http.StatusNoContent)
}

// simple sends a small request and wants exactly the status want.
func (c *Client) simple(
	ctx context.Context,
	op, method, target string,
	timeout time.Duration,
	payload []byte,
	want int,
) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := c.newRequest(ctx, method, target, body)
	if err != nil {
		return err
	}

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != want {
		return statusError(op, resp.StatusCode, readReply(resp))
	}

	return nil
}

func (c *Client) recordingURL(id int64) string {
	return c.base + "/mic/v1/recordings/" + strconv.FormatInt(id, 10)
}

func (c *Client) newRequest(ctx context.Context, method, target string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("recorder: build %s request: %w", method, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)

	return req, nil
}

// do sends req and returns the response; transport errors drop the URL's
// userinfo-free detail but never carry the token (it only sits in a header).
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("recorder: %s %s: %w", req.Method, req.URL.Path, err)
	}

	return resp, nil
}
