// Package memos is a thin client for the Memos REST API (v0.31.0 under
// /api/v1, 26.10+ under /api). It writes notes and attachments; it also lists the
// inbox for the digest, reusing the prefix and owner cache.
package memos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

const (
	defaultTimeout = 30 * time.Second
	// maxBody is the largest response body read; anything longer fails
	// with "response too large".
	maxBody       = 1 << 20
	prefixAuto    = "auto"
	prefixCurrent = "/api"
	prefixLegacy  = "/api/v1"
	memoPrefix    = "memos/"
	// grpcAlreadyExists is the gRPC-gateway status code for AlreadyExists.
	grpcAlreadyExists = 6
)

// Config configures a Client.
type Config struct {
	// BaseURL is the internal Memos URL, for example http://memos.example.org:5230.
	BaseURL string
	// PublicURL is the base of links shown to the owner; empty means BaseURL.
	PublicURL string
	// Token is the Personal Access Token. It only ever goes into the
	// Authorization header.
	Token string
	// APIPrefix is "auto" (or empty), "/api" or "/api/v1".
	APIPrefix string
	// InboxTag is the tag the inbox listing filters on; empty means "inbox".
	InboxTag string
	// Version is used for "User-Agent: VoiceNotes/<version>".
	Version string
}

// Note is one note to create. Content is already rendered; the client does not
// render.
type Note struct {
	// CaptureID derives the deterministic memoId.
	CaptureID  int64
	Content    string
	ReceivedAt time.Time
}

// MemoID returns the deterministic memo uid for a capture:
// "vn-<id>-<shorthash>", where shorthash is the first 8 hex digits of
// sha256("<id>|<receivedAt as UTC unix nanoseconds>"). Hashing the arrival time
// as well as the id keeps a reset database, which restarts ids, from colliding
// with notes written earlier. The result is at most 31 characters and matches
// ^[a-zA-Z0-9]([a-zA-Z0-9-]{0,34}[a-zA-Z0-9])?$.
func MemoID(captureID int64, receivedAt time.Time) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d|%d", captureID, receivedAt.UTC().UnixNano()))

	return fmt.Sprintf("vn-%d-%s", captureID, hex.EncodeToString(sum[:])[:8])
}

// Client calls the Memos REST API.
type Client struct {
	cfg    Config
	client *http.Client

	mu     sync.Mutex
	prefix string
	owner  string
}

// NewClient builds a Client. A nil client gets a default one with a 30s
// timeout; callers pass their own to inject tests.
func NewClient(cfg Config, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	if cfg.APIPrefix == "" {
		cfg.APIPrefix = prefixAuto
	}
	c := &Client{cfg: cfg, client: client}
	if cfg.APIPrefix != prefixAuto {
		c.prefix = cfg.APIPrefix
	}

	return c
}

// Create writes the note and returns its name ("memos/<uid>") and link. A memo
// that already exists (a retried create) counts as success. Every non-OK
// outcome is tagged capture.WaitingMemos; reasons never contain the token or
// response bodies.
func (c *Client) Create(ctx context.Context, n Note) (string, string, capture.Outcome) {
	prefix, out := c.resolvePrefix(ctx)
	if out.Class != capture.ClassOK {
		return "", "", out
	}
	uid := MemoID(n.CaptureID, n.ReceivedAt)
	body := map[string]string{
		"content":    n.Content,
		"visibility": "PRIVATE",
		"createTime": n.ReceivedAt.UTC().Format(time.RFC3339),
	}
	status, raw, out := c.do(ctx, http.MethodPost, prefix+"/memos?memoId="+url.QueryEscape(uid), body)
	if out.Class != capture.ClassOK {
		return "", "", out
	}
	switch {
	case status >= 200 && status < 300:
		var m struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &m); err != nil || !validName(m.Name) {
			return "", "", capture.Temporary("malformed response").On(capture.WaitingMemos)
		}

		return m.Name, c.link(m.Name), capture.OK()
	case alreadyExists(status, raw):
		return c.fetchExisting(ctx, prefix, uid)
	default:
		// A duplicate uid may surface as any error (for example a 5xx from a
		// unique-constraint violation), so look the memo up before giving up.
		// If it is not there, or the lookup fails, keep the create's outcome.
		if name, link, found := c.fetchExisting(ctx, prefix, uid); found.Class == capture.ClassOK {
			return name, link, found
		}

		return "", "", c.classify(status)
	}
}

// Attach uploads the file inline (base64) to the memo, unless the memo already
// lists an attachment with that filename, which makes the step idempotent.
func (c *Client) Attach(ctx context.Context, name, filename, mime string, data []byte) capture.Outcome {
	if !validName(name) {
		return capture.Permanent("invalid memo name").On(capture.WaitingMemos)
	}
	prefix, out := c.resolvePrefix(ctx)
	if out.Class != capture.ClassOK {
		return out
	}
	has, out := c.hasAttachment(ctx, prefix, name, filename)
	if out.Class != capture.ClassOK || has {
		return out
	}

	body := map[string]string{
		"filename": filename,
		"type":     mime,
		"content":  base64.StdEncoding.EncodeToString(data),
		"memo":     name,
	}
	status, _, out := c.do(ctx, http.MethodPost, prefix+"/attachments", body)
	if out.Class != capture.ClassOK {
		return out
	}
	if status < 200 || status >= 300 {
		return c.classify(status)
	}

	return capture.OK()
}

// Owner returns the token owner's user name (for example "users/1") from
// GET {P}/auth/me. It is cached after the first success, and is what the inbox
// listing later filters on (research R8).
func (c *Client) Owner(ctx context.Context) (string, capture.Outcome) {
	prefix, out := c.resolvePrefix(ctx)
	if out.Class != capture.ClassOK {
		return "", out
	}
	c.mu.Lock()
	owner := c.owner
	c.mu.Unlock()
	if owner != "" {
		return owner, capture.OK()
	}
	// An explicit prefix skips the probe, so the owner is fetched on demand.
	status, raw, out := c.do(ctx, http.MethodGet, prefix+"/auth/me", nil)
	if out.Class != capture.ClassOK {
		return "", out
	}
	if status < 200 || status >= 300 {
		return "", c.classify(status)
	}
	name, ok := parseOwner(raw)
	if !ok {
		return "", capture.Temporary("malformed response").On(capture.WaitingMemos)
	}
	c.mu.Lock()
	c.owner = name
	c.mu.Unlock()

	return name, capture.OK()
}

func parseOwner(raw []byte) (string, bool) {
	var me struct {
		User struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &me); err != nil || me.User.Name == "" {
		return "", false
	}

	return me.User.Name, true
}

// hasAttachment reports whether the memo already lists an attachment named
// filename.
func (c *Client) hasAttachment(ctx context.Context, prefix, name, filename string) (bool, capture.Outcome) {
	status, raw, out := c.do(ctx, http.MethodGet, prefix+"/"+escapeName(name), nil)
	if out.Class != capture.ClassOK {
		return false, out
	}
	if status < 200 || status >= 300 {
		return false, c.classify(status)
	}
	var m struct {
		Attachments []struct {
			Filename string `json:"filename"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false, capture.Temporary("malformed response").On(capture.WaitingMemos)
	}
	for _, a := range m.Attachments {
		if a.Filename == filename {
			return true, capture.OK()
		}
	}

	return false, capture.OK()
}

func (c *Client) fetchExisting(ctx context.Context, prefix, uid string) (string, string, capture.Outcome) {
	status, raw, out := c.do(ctx, http.MethodGet, prefix+"/memos/"+url.PathEscape(uid), nil)
	if out.Class != capture.ClassOK {
		return "", "", out
	}
	if status < 200 || status >= 300 {
		return "", "", c.classify(status)
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || !validName(m.Name) {
		return "", "", capture.Temporary("malformed response").On(capture.WaitingMemos)
	}

	return m.Name, c.link(m.Name), capture.OK()
}

// resolvePrefix returns the API prefix, probing GET /api/auth/me and then
// /api/v1/auth/me (on 404 or 410) in auto mode. Only a successful probe is
// cached, together with the owner's user name; a failed one is retried on the
// next call.
func (c *Client) resolvePrefix(ctx context.Context) (string, capture.Outcome) {
	c.mu.Lock()
	p := c.prefix
	c.mu.Unlock()
	if p != "" {
		return p, capture.OK()
	}

	var out capture.Outcome
	for _, cand := range []string{prefixCurrent, prefixLegacy} {
		status, raw, o := c.do(ctx, http.MethodGet, cand+"/auth/me", nil)
		if o.Class != capture.ClassOK {
			return "", o
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			out = c.classify(status)
			continue
		}
		if status < 200 || status >= 300 {
			return "", c.classify(status)
		}
		name, ok := parseOwner(raw)
		if !ok {
			// A 2xx that is not the auth/me shape (for example an SPA fallback)
			// means "not this prefix".
			out = capture.Temporary("malformed response").On(capture.WaitingMemos)
			continue
		}
		c.mu.Lock()
		c.prefix, c.owner = cand, name
		c.mu.Unlock()

		return cand, capture.OK()
	}

	return "", out
}

// do sends one request. The returned outcome is non-OK only for transport and
// build failures; the caller classifies the status. A 404 or 410 on an
// automatically resolved prefix drops the cache so the next call probes again.
func (c *Client) do(ctx context.Context, method, path string, body any) (int, []byte, capture.Outcome) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, capture.Permanent("build request: " + err.Error()).On(capture.WaitingMemos)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, capture.Misconfig("invalid MEMOS_URL").On(capture.WaitingMemos)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("User-Agent", "VoiceNotes/"+c.cfg.Version)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, capture.NetworkError(err).On(capture.WaitingMemos)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, out := readBody(resp.Body)
	if out.Class != capture.ClassOK {
		return 0, nil, out
	}
	gone := resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound
	if gone && c.cfg.APIPrefix == prefixAuto {
		c.mu.Lock()
		c.prefix, c.owner = "", ""
		c.mu.Unlock()
	}

	return resp.StatusCode, raw, capture.OK()
}

// readBody reads at most maxBody bytes. It reads one byte past the limit so
// an oversized body is reported as "response too large" instead of being
// truncated and then failing as malformed JSON.
func readBody(r io.Reader) ([]byte, capture.Outcome) {
	raw, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, capture.NetworkError(err).On(capture.WaitingMemos)
	}
	if len(raw) > maxBody {
		return nil, capture.Temporary("response too large").On(capture.WaitingMemos)
	}

	return raw, capture.OK()
}

func (c *Client) classify(status int) capture.Outcome {
	return capture.ClassifyHTTP(status).On(capture.WaitingMemos)
}

func (c *Client) link(name string) string {
	base := c.cfg.PublicURL
	if base == "" {
		base = c.cfg.BaseURL
	}

	return strings.TrimRight(base, "/") + "/" + name
}

// alreadyExists recognises the duplicate-memoId error: HTTP 409, or any error
// status whose gRPC-gateway body carries code 6 (AlreadyExists) or says
// "already exists" (the exact status Memos uses is unverified).
func alreadyExists(status int, raw []byte) bool {
	if status == http.StatusConflict {
		return true
	}
	if status < 400 {
		return false
	}
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Code == grpcAlreadyExists {
		return true
	}
	lower := strings.ToLower(string(raw))

	return strings.Contains(lower, "already exists") || strings.Contains(lower, "alreadyexists")
}

func validName(name string) bool {
	uid, ok := strings.CutPrefix(name, memoPrefix)

	return ok && uid != "" && !strings.Contains(uid, "/")
}

func escapeName(name string) string {
	return memoPrefix + url.PathEscape(strings.TrimPrefix(name, memoPrefix))
}
