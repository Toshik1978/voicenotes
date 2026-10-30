//go:build integration

package memos

// Opt-in integration test against a real, freshly started Memos server
// (quickstart scenario 13 and research R7/R8). It is compiled only with the
// `integration` build tag, so the default suite never lists it as skipped,
// and it still skips unless VOICENOTES_IT_MEMOS_URL is set, for example:
//
//	docker run -d --rm --name vn-it-memos -p 127.0.0.1:5230:5230 neosmemo/memos:0.31.0
//	VOICENOTES_IT_MEMOS_URL=http://localhost:5230 go test -tags integration -run Integration -v ./internal/memos
//
// The server must have no users yet: the test signs up the first user (who
// becomes the admin owner), creates a Personal Access Token for it and a second
// ordinary user, all through the Memos API with random passwords. Nothing is
// read from or written to disk. VOICENOTES_IT_MEMOS_WANT_PREFIX ("/api" or
// "/api/v1"), when set, is the prefix the auto-probe must pick.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

const (
	itURLEnv    = "VOICENOTES_IT_MEMOS_URL"
	itPrefixEnv = "VOICENOTES_IT_MEMOS_WANT_PREFIX"
	itOwner     = "vn-it-owner"
	itOther     = "vn-it-other"
)

// itServer talks to the real Memos API directly, outside the client under
// test, to bootstrap users and to check what the client wrote.
type itServer struct {
	t      *testing.T
	base   string
	prefix string
	http   *http.Client
}

func (s *itServer) call(method, path, bearer string, body any) (int, map[string]any) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(s.t.Context(), method, s.base+s.prefix+path, rd)
	if err != nil {
		s.t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("read %s %s: %v", method, path, err)
	}
	var out map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &out) != nil {
		out = map[string]any{"raw": string(raw)}
	}

	return resp.StatusCode, out
}

func (s *itServer) mustCall(method, path, bearer string, body any) map[string]any {
	s.t.Helper()
	status, out := s.call(method, path, bearer, body)
	if status < 200 || status >= 300 {
		s.t.Fatalf("%s %s: status %d: %v", method, path, status, out)
	}

	return out
}

// detectPrefix finds the API prefix from the unauthenticated instance profile:
// 26.10+ serves it under /api, v0.31.0 under /api/v1.
func (s *itServer) detectPrefix() {
	s.t.Helper()
	for _, p := range []string{prefixCurrent, prefixLegacy} {
		s.prefix = p
		if status, out := s.call(http.MethodGet, "/instance/profile", "", nil); status == http.StatusOK &&
			out["version"] != nil {
			s.t.Logf("memos version %v under %s", out["version"], p)

			return
		}
	}
	s.t.Fatal("no Memos instance profile under /api or /api/v1")
}

func randomSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}

	return hex.EncodeToString(b)
}

func (s *itServer) signIn(user, password string) string {
	s.t.Helper()
	out := s.mustCall(http.MethodPost, "/auth/signin", "", map[string]any{
		"passwordCredentials": map[string]string{"username": user, "password": password},
	})
	tok, _ := out["accessToken"].(string)
	if tok == "" {
		s.t.Fatalf("sign in %s: no access token", user)
	}

	return tok
}

// bootstrap creates the owner (first sign-up, so admin), its PAT and a second
// user. It returns the owner's user name, the PAT and the other user's access
// token.
func (s *itServer) bootstrap() (string, string, string) {
	s.t.Helper()
	profile := s.mustCall(http.MethodGet, "/instance/profile", "", nil)
	if needsSetup, known := profile["needsSetup"].(bool); profile["admin"] != nil || (known && !needsSetup) {
		s.t.Fatalf("%s must point at a fresh Memos with no users", itURLEnv)
	}
	ownerPass, otherPass := randomSecret(s.t), randomSecret(s.t)
	owner := s.mustCall(http.MethodPost, "/users", "", map[string]string{
		"username": itOwner, "password": ownerPass, "role": "ADMIN",
	})
	ownerName, _ := owner["name"].(string)
	adminTok := s.signIn(itOwner, ownerPass)
	pat := s.mustCall(http.MethodPost, "/"+ownerName+"/personalAccessTokens", adminTok, map[string]any{
		"description": "voicenotes integration test", "expiresInDays": 0,
	})
	patTok, _ := pat["token"].(string)
	if !strings.HasPrefix(patTok, "memos_pat_") {
		s.t.Fatal("personal access token missing from the response")
	}
	s.mustCall(http.MethodPost, "/users", adminTok, map[string]string{
		"username": itOther, "password": otherPass, "role": "USER",
	})

	return ownerName, patTok, s.signIn(itOther, otherPass)
}

func (s *itServer) memo(name, token string) map[string]any {
	s.t.Helper()

	return s.mustCall(http.MethodGet, "/"+name, token, nil)
}

// memoNames lists the memo names a token can read, optionally filtered.
func (s *itServer) memoNames(token, filter string) []string {
	s.t.Helper()
	q := url.Values{}
	q.Set("pageSize", "1000")
	if filter != "" {
		q.Set("filter", filter)
	}
	out := s.mustCall(http.MethodGet, "/memos?"+q.Encode(), token, nil)
	list, _ := out["memos"].([]any)
	names := make([]string, 0, len(list))
	for _, m := range list {
		if mm, ok := m.(map[string]any); ok {
			name, _ := mm["name"].(string)
			names = append(names, name)
		}
	}
	slices.Sort(names)

	return names
}

func TestIntegrationRealMemos(t *testing.T) {
	base := strings.TrimRight(os.Getenv(itURLEnv), "/")
	if base == "" {
		t.Skipf("set %s to a fresh Memos server to run", itURLEnv)
	}
	srv := &itServer{t: t, base: base, http: &http.Client{Timeout: 30 * time.Second}}
	srv.detectPrefix()
	if want := os.Getenv(itPrefixEnv); want != "" && want != srv.prefix {
		t.Fatalf("server prefix %s, want %s", srv.prefix, want)
	}
	ownerName, pat, otherTok := srv.bootstrap()
	newIT := func() *Client {
		return NewClient(Config{BaseURL: base, Token: pat, APIPrefix: prefixAuto, Version: "it"}, nil)
	}

	// Prefix auto-probe and auth/me.
	c := newIT()
	owner, out := c.Owner(t.Context())
	if out.Class != capture.ClassOK || owner != ownerName {
		t.Fatalf("Owner() = %q, %+v; want %q", owner, out, ownerName)
	}
	if c.prefix != srv.prefix {
		t.Fatalf("auto-probe picked %q, server serves %q", c.prefix, srv.prefix)
	}

	received := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	name := itCheckCreate(t, srv, c, newIT(), ownerName, pat, received)
	itCheckAttach(t, srv, c, name, pat)
	itCheckOwnerFilter(t, srv, c, newIT(), itInbox{
		owner: ownerName, pat: pat, otherTok: otherTok, taskNote: name, received: received,
	})
}

// itCheckCreate creates a task note, retries it on the same and on a fresh
// client, and checks what Memos stored: PRIVATE, inline tags, createTime.
func itCheckCreate(t *testing.T, srv *itServer, c, fresh *Client, owner, pat string, received time.Time) string {
	t.Helper()
	task := Note{
		CaptureID:  1,
		Content:    "Integration voice note\n\n- [ ] call the dentist\n\n#inbox #todo",
		ReceivedAt: received,
	}
	name, link, out := c.Create(t.Context(), task)
	if out.Class != capture.ClassOK || name != "memos/"+MemoID(1, received) || link != srv.base+"/"+name {
		t.Fatalf("Create = %q, %q, %+v", name, link, out)
	}
	for _, retry := range []*Client{c, fresh} {
		again, _, retryOut := retry.Create(t.Context(), task)
		if retryOut.Class != capture.ClassOK || again != name {
			t.Fatalf("retried Create = %q, %+v; want %q", again, retryOut, name)
		}
	}
	m := srv.memo(name, pat)
	if m["visibility"] != "PRIVATE" || m["creator"] != owner {
		t.Fatalf("memo visibility/creator = %v/%v", m["visibility"], m["creator"])
	}
	created, _ := m["createTime"].(string)
	if got, err := time.Parse(time.RFC3339, created); err != nil || !got.Equal(received) {
		t.Fatalf("createTime = %q, want %v", created, received)
	}
	tags, _ := m["tags"].([]any)
	if !slices.Contains(tags, any("inbox")) || !slices.Contains(tags, any("todo")) {
		t.Fatalf("tags = %v, want inbox and todo", tags)
	}

	return name
}

// itCheckAttach uploads the audio twice and expects exactly one attachment
// linked to the memo.
func itCheckAttach(t *testing.T, srv *itServer, c *Client, name, pat string) {
	t.Helper()
	const filename = "voice-2026-03-04.ogg"
	audio := []byte("OggS\x00fake-opus-payload-for-the-integration-test")
	for range 2 {
		if out := c.Attach(t.Context(), name, filename, "audio/ogg", audio); out.Class != capture.ClassOK {
			t.Fatalf("Attach: %+v", out)
		}
	}
	atts, _ := srv.memo(name, pat)["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments = %v, want exactly one", atts)
	}
	if a, _ := atts[0].(map[string]any); a["filename"] != filename || a["type"] != "audio/ogg" {
		t.Fatalf("attachment = %v", a)
	}
}

type itInbox struct {
	owner, pat, otherTok, taskNote string
	received                       time.Time
}

// itCheckOwnerFilter is the research R8 check: a second user's PUBLIC #inbox
// note is visible to the owner's token, so only the creator filter keeps it
// out of ListInbox.
func itCheckOwnerFilter(t *testing.T, srv *itServer, c, fresh *Client, in itInbox) {
	t.Helper()
	plain, _, out := c.Create(t.Context(), Note{
		CaptureID: 2, Content: "Reviewed already", ReceivedAt: in.received.Add(time.Minute),
	})
	if out.Class != capture.ClassOK {
		t.Fatalf("Create plain: %+v", out)
	}
	text, _, out := c.Create(t.Context(), Note{
		CaptureID: 3, Content: "Text capture\n\n#inbox", ReceivedAt: in.received.Add(2 * time.Minute),
	})
	if out.Class != capture.ClassOK {
		t.Fatalf("Create text: %+v", out)
	}
	foreign := srv.mustCall(http.MethodPost, "/memos", in.otherTok, map[string]string{
		"content": "Someone else's note #inbox", "visibility": "PUBLIC",
	})
	foreignName, _ := foreign["name"].(string)
	if visible := srv.memoNames(in.pat, ""); !slices.Contains(visible, foreignName) {
		t.Fatalf("owner token cannot see %s (%v); the R8 check would prove nothing", foreignName, visible)
	}
	mine := srv.memoNames(in.pat, `creator == "`+in.owner+`"`)
	if want := sorted(in.taskNote, plain, text); !slices.Equal(mine, want) {
		t.Fatalf("owner memos = %v, want %v; a retried create must not duplicate", mine, want)
	}

	notes, err := fresh.ListInbox(t.Context())
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(notes) != 2 || notes[0].Name != in.taskNote || notes[1].Name != text {
		t.Fatalf("ListInbox = %+v; want %s then %s", notes, in.taskNote, text)
	}
	if !notes[0].HasTaskList || notes[1].HasTaskList || !notes[0].CreateTime.Equal(in.received) {
		t.Fatalf("ListInbox task markers/times = %+v", notes)
	}
}

func sorted(s ...string) []string {
	slices.Sort(s)

	return s
}
