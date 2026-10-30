package memos

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

const fakeToken = "memos_pat_FAKE-TOKEN"

var memoIDRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,34}[a-zA-Z0-9])?$`)

type attView struct {
	Filename string `json:"filename"`
}

type memoView struct {
	Name        string    `json:"name"`
	Attachments []attView `json:"attachments"`
}

// fakeMemos is an in-memory Memos server. current selects which prefix is
// served; the other answers legacyStatus (404 or 410).
type fakeMemos struct {
	mu           sync.Mutex
	prefix       string
	legacyStatus int
	legacyBody   string // when set, the other prefix answers 200 with this body
	memos        map[string]map[string]any
	attachments  map[string][]string
	calls        []string
	probeStatus  int // when set, auth/me answers this status instead
	createStatus int // when set, create answers this status
	createBody   string
	lastAuth     string
	lastUA       string
	lastCreate   map[string]any
	lastAttach   map[string]any
}

func newFake(prefix string) *fakeMemos {
	return &fakeMemos{
		prefix: prefix, legacyStatus: http.StatusNotFound,
		memos: map[string]map[string]any{}, attachments: map[string][]string{},
	}
}

func (f *fakeMemos) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.lastAuth, f.lastUA = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
	p, ok := strings.CutPrefix(r.URL.Path, f.prefix)
	if !ok || (f.prefix == "/api" && strings.HasPrefix(r.URL.Path, "/api/v1")) {
		if f.legacyBody != "" {
			_, _ = io.WriteString(w, f.legacyBody)
			return
		}
		w.WriteHeader(f.legacyStatus)

		return
	}
	switch {
	case p == "/auth/me":
		if f.probeStatus != 0 {
			w.WriteHeader(f.probeStatus)
			return
		}
		_, _ = io.WriteString(w, `{"user":{"name":"users/7"}}`)
	case p == "/memos" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastCreate = body
		id := r.URL.Query().Get("memoId")
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			_, _ = io.WriteString(w, f.createBody)
			return
		}
		if _, dup := f.memos[id]; dup {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"code":6,"message":"memo already exists"}`)
			return
		}
		f.memos[id] = body
		_, _ = io.WriteString(w, `{"name":"memos/`+id+`"}`)
	case strings.HasPrefix(p, "/memos/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(p, "/memos/")
		if _, ok := f.memos[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var views []attView
		for _, fn := range f.attachments["memos/"+id] {
			views = append(views, attView{fn})
		}
		if err := json.NewEncoder(w).Encode(memoView{Name: "memos/" + id, Attachments: views}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case p == "/attachments" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastAttach = body
		memo, _ := body["memo"].(string)
		fn, _ := body["filename"].(string)
		f.attachments[memo] = append(f.attachments[memo], fn)
		_, _ = io.WriteString(w, `{"name":"attachments/1"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeMemos) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}

	return n
}

func newClient(t *testing.T, f *fakeMemos, mutate func(*Config)) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := Config{
		BaseURL: srv.URL, PublicURL: "https://memos.example.org/", Token: fakeToken,
		APIPrefix: "auto", Version: "1.2.3",
	}
	if mutate != nil {
		mutate(&cfg)
	}

	return NewClient(cfg, srv.Client())
}

func ctx() context.Context { return context.Background() }

func create(c *Client, n Note) capture.Outcome {
	_, _, out := c.Create(ctx(), n)

	return out
}

func receivedAt() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }

func TestMemoID(t *testing.T) {
	for _, id := range []int64{1, 42, 9223372036854775807} {
		got := MemoID(id, receivedAt())
		if !memoIDRe.MatchString(got) || len(got) > 36 {
			t.Errorf("MemoID(%d) = %q violates the regex or length", id, got)
		}
		if got != MemoID(id, receivedAt()) {
			t.Errorf("MemoID(%d) is not deterministic", id)
		}
		if !strings.HasPrefix(got, "vn-") {
			t.Errorf("MemoID(%d) = %q lacks the vn- prefix", id, got)
		}
	}
	if MemoID(1, receivedAt()) == MemoID(2, receivedAt()) ||
		MemoID(1, receivedAt()) == MemoID(1, receivedAt().Add(time.Second)) {
		t.Error("distinct captures must get distinct ids")
	}
	if MemoID(1, receivedAt()) != MemoID(1, receivedAt().In(time.FixedZone("x", 3600))) {
		t.Error("the id must not depend on the time zone")
	}
}

func TestPrefixes(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		legacyCode int
		cfgPrefix  string
		wantProbes []string
	}{
		{"auto 26.10", "/api", 404, "auto", []string{"GET /api/auth/me"}},
		{"auto v0.31 via 404", "/api/v1", 404, "auto", []string{"GET /api/auth/me", "GET /api/v1/auth/me"}},
		{"auto v0.31 via 410", "/api/v1", 410, "", []string{"GET /api/auth/me", "GET /api/v1/auth/me"}},
		{"explicit v1 skips probe", "/api/v1", 404, "/api/v1", nil},
		{"explicit api skips probe", "/api", 404, "/api", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(tt.prefix)
			f.legacyStatus = tt.legacyCode
			c := newClient(t, f, func(cfg *Config) { cfg.APIPrefix = tt.cfgPrefix })
			name, link, out := c.Create(
				ctx(),
				Note{CaptureID: 5, Content: "hi #inbox", ReceivedAt: receivedAt()},
			)
			if out.Class != capture.ClassOK {
				t.Fatalf("outcome = %+v", out)
			}
			uid := MemoID(5, receivedAt())
			if name != "memos/"+uid || link != "https://memos.example.org/memos/"+uid {
				t.Errorf("name=%q link=%q", name, link)
			}
			if f.count("POST "+tt.prefix+"/memos") != 1 {
				t.Errorf("calls = %v", f.calls)
			}
			for _, p := range tt.wantProbes {
				if f.count(p) != 1 {
					t.Errorf("probe %q count = %d; calls = %v", p, f.count(p), f.calls)
				}
			}
			if tt.wantProbes == nil && f.count(tt.prefix+"/auth/me") != 0 {
				t.Errorf("explicit prefix probed: %v", f.calls)
			}
			// The resolution is cached.
			if out := create(c, Note{CaptureID: 6, ReceivedAt: receivedAt()}); out.Class != capture.ClassOK {
				t.Fatalf("second create = %+v", out)
			}
			if f.count("GET /api/auth/me") > 1 {
				t.Errorf("probe repeated: %v", f.calls)
			}
		})
	}
}

func TestOwnerCached(t *testing.T) {
	f := newFake("/api/v1")
	c := newClient(t, f, nil)
	for range 2 {
		owner, out := c.Owner(ctx())
		if out.Class != capture.ClassOK || owner != "users/7" {
			t.Fatalf("owner=%q out=%+v", owner, out)
		}
	}
	if f.count("GET /api/v1/auth/me") != 1 {
		t.Errorf("owner fetched more than once: %v", f.calls)
	}

	f2 := newFake("/api")
	c2 := newClient(t, f2, func(cfg *Config) { cfg.APIPrefix = "/api" })
	if owner, out := c2.Owner(ctx()); out.Class != capture.ClassOK || owner != "users/7" {
		t.Fatalf("explicit prefix owner=%q out=%+v", owner, out)
	}
}

func TestFailedProbeNotCached(t *testing.T) {
	f := newFake("/api/v1")
	f.probeStatus = http.StatusServiceUnavailable
	c := newClient(t, f, nil)
	_, _, out := c.Create(ctx(), Note{CaptureID: 1, ReceivedAt: receivedAt()})
	if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingMemos {
		t.Fatalf("outcome = %+v", out)
	}
	f.mu.Lock()
	f.probeStatus = 0
	f.mu.Unlock()
	if out := create(c, Note{CaptureID: 1, ReceivedAt: receivedAt()}); out.Class != capture.ClassOK {
		t.Fatalf("retry after failed probe = %+v", out)
	}
}

func TestProbeBothMissing(t *testing.T) {
	f := newFake("/nowhere")
	c := newClient(t, f, nil)
	_, _, out := c.Create(ctx(), Note{CaptureID: 1, ReceivedAt: receivedAt()})
	if out.Class != capture.ClassMisconfig || out.WaitingOn != capture.WaitingMemos {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestCreateRequest(t *testing.T) {
	f := newFake("/api/v1")
	c := newClient(t, f, nil)
	_, _, out := c.Create(
		ctx(),
		Note{CaptureID: 9, Content: "body #inbox", ReceivedAt: receivedAt().In(time.FixedZone("x", 7200))},
	)
	if out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v", out)
	}
	if f.lastAuth != "Bearer "+fakeToken || f.lastUA != "VoiceNotes/1.2.3" {
		t.Errorf("auth=%q ua=%q", f.lastAuth, f.lastUA)
	}
	want := map[string]any{"content": "body #inbox", "visibility": "PRIVATE", "createTime": "2026-03-04T05:06:07Z"}
	for k, v := range want {
		if f.lastCreate[k] != v {
			t.Errorf("%s = %v, want %v", k, f.lastCreate[k], v)
		}
	}
}

func TestCreateAlreadyExists(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"409", 409, ``},
		{"grpc code 6", 500, `{"code":6,"message":"x"}`},
		{"message text", 400, `{"message":"memo ALREADY EXISTS"}`},
		{"AlreadyExists text", 500, `rpc error: code = AlreadyExists`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake("/api/v1")
			uid := MemoID(3, receivedAt())
			f.memos[uid] = map[string]any{}
			f.createStatus, f.createBody = tt.status, tt.body
			c := newClient(t, f, nil)
			name, link, out := c.Create(ctx(), Note{CaptureID: 3, ReceivedAt: receivedAt()})
			if out.Class != capture.ClassOK || name != "memos/"+uid || link != "https://memos.example.org/memos/"+uid {
				t.Fatalf("name=%q link=%q out=%+v", name, link, out)
			}
			if f.count("GET /api/v1/memos/"+uid) != 1 {
				t.Errorf("existing memo not fetched: %v", f.calls)
			}
		})
	}
}

func TestCreateIdempotentRetry(t *testing.T) {
	f := newFake("/api")
	c := newClient(t, f, nil)
	n := Note{CaptureID: 4, Content: "x", ReceivedAt: receivedAt()}
	n1, l1, o1 := c.Create(ctx(), n)
	n2, l2, o2 := c.Create(ctx(), n)
	if o1.Class != capture.ClassOK || o2.Class != capture.ClassOK || n1 != n2 || l1 != l2 || len(f.memos) != 1 {
		t.Fatalf("(%q,%q,%+v) (%q,%q,%+v) memos=%d", n1, l1, o1, n2, l2, o2, len(f.memos))
	}
}

func TestCreateExistsButMissing(t *testing.T) {
	f := newFake("/api")
	f.createStatus = 409
	c := newClient(t, f, nil)
	_, _, out := c.Create(ctx(), Note{CaptureID: 3, ReceivedAt: receivedAt()})
	if out.Class != capture.ClassMisconfig || out.WaitingOn != capture.WaitingMemos {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestCreateClassification(t *testing.T) {
	tests := []struct {
		status int
		body   string
		want   capture.Class
	}{
		{500, `boom`, capture.ClassTemporary},
		{429, ``, capture.ClassTemporary},
		{401, ``, capture.ClassMisconfig},
		{403, ``, capture.ClassMisconfig},
		{400, `{"message":"content too long"}`, capture.ClassPermanent},
		{413, ``, capture.ClassPermanent},
	}
	for _, tt := range tests {
		f := newFake("/api")
		f.createStatus, f.createBody = tt.status, tt.body
		c := newClient(t, f, nil)
		_, _, out := c.Create(ctx(), Note{CaptureID: 1, ReceivedAt: receivedAt()})
		if out.Class != tt.want || out.WaitingOn != capture.WaitingMemos {
			t.Errorf("status %d: outcome = %+v", tt.status, out)
		}
		if strings.Contains(out.Reason, fakeToken) || strings.Contains(out.Reason, "content too long") {
			t.Errorf("status %d: reason leaks: %q", tt.status, out.Reason)
		}
	}
}

func TestCreateMalformedResponse(t *testing.T) {
	f := newFake("/api")
	f.createStatus, f.createBody = 200, `{"name":"nonsense"}`
	c := newClient(t, f, nil)
	_, _, out := c.Create(ctx(), Note{CaptureID: 1, ReceivedAt: receivedAt()})
	if out.Class != capture.ClassTemporary {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestNetworkErrorOmitsSecrets(t *testing.T) {
	c := NewClient(
		Config{BaseURL: "http://user:secret@127.0.0.1:1", Token: fakeToken, APIPrefix: "/api"},
		&http.Client{Timeout: time.Second},
	)
	_, _, out := c.Create(ctx(), Note{CaptureID: 1, ReceivedAt: receivedAt()})
	if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingMemos {
		t.Fatalf("outcome = %+v", out)
	}
	for _, leak := range []string{"user", "secret", fakeToken} {
		if out.Reason == "" || strings.Contains(out.Reason, leak) {
			t.Errorf("reason = %q", out.Reason)
		}
	}
}

func TestInvalidBaseURL(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://[::1", Token: fakeToken, APIPrefix: "/api"}, nil)
	_, _, out := c.Create(ctx(), Note{CaptureID: 1, ReceivedAt: receivedAt()})
	if out.Class != capture.ClassMisconfig || out.WaitingOn != capture.WaitingMemos {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestPublicURLDefaultsToBaseURL(t *testing.T) {
	f := newFake("/api")
	c := newClient(t, f, func(cfg *Config) { cfg.PublicURL = "" })
	_, link, _ := c.Create(ctx(), Note{CaptureID: 1, ReceivedAt: receivedAt()})
	if !strings.HasPrefix(link, "http://127.0.0.1:") || !strings.HasSuffix(link, "/memos/"+MemoID(1, receivedAt())) {
		t.Errorf("link = %q", link)
	}
}

func TestGoneDropsCachedPrefix(t *testing.T) {
	f := newFake("/api/v1")
	f.legacyStatus = 410
	c := newClient(t, f, nil)
	if out := create(c, Note{CaptureID: 1, ReceivedAt: receivedAt()}); out.Class != capture.ClassOK {
		t.Fatalf("first create = %+v", out)
	}
	// The instance is upgraded to 26.10: the old prefix now answers 410.
	f.mu.Lock()
	f.prefix = "/api"
	f.mu.Unlock()
	_, _, out := c.Create(ctx(), Note{CaptureID: 2, ReceivedAt: receivedAt()})
	if out.Class == capture.ClassOK {
		t.Fatalf("expected a failure on the stale prefix, got %+v", out)
	}
	if out := create(c, Note{CaptureID: 2, ReceivedAt: receivedAt()}); out.Class != capture.ClassOK {
		t.Fatalf("create after re-probe = %+v", out)
	}
}

func TestAttach(t *testing.T) {
	f := newFake("/api/v1")
	uid := MemoID(1, receivedAt())
	f.memos[uid] = map[string]any{}
	c := newClient(t, f, nil)
	audio := []byte("OggS\x00\xff")
	out := c.Attach(ctx(), "memos/"+uid, "voice-2026-03-04.ogg", "audio/ogg", audio)
	if out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v", out)
	}
	enc, _ := f.lastAttach["content"].(string)
	got, _ := base64.StdEncoding.DecodeString(enc)
	if !bytes.Equal(got, audio) || f.lastAttach["memo"] != "memos/"+uid ||
		f.lastAttach["filename"] != "voice-2026-03-04.ogg" || f.lastAttach["type"] != "audio/ogg" {
		t.Errorf("attach body = %v", f.lastAttach)
	}
	// Second attach is skipped: the memo already lists the filename.
	out = c.Attach(ctx(), "memos/"+uid, "voice-2026-03-04.ogg", "audio/ogg", audio)
	if out.Class != capture.ClassOK {
		t.Fatalf("second attach = %+v", out)
	}
	if f.count("POST /api/v1/attachments") != 1 {
		t.Errorf("attachment uploaded twice: %v", f.calls)
	}
	// A different filename is uploaded.
	out = c.Attach(ctx(), "memos/"+uid, "other.ogg", "audio/ogg", audio)
	if out.Class != capture.ClassOK {
		t.Fatalf("other attach = %+v", out)
	}
	if f.count("POST /api/v1/attachments") != 2 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestAttachErrors(t *testing.T) {
	f := newFake("/api")
	c := newClient(t, f, nil)
	out := c.Attach(ctx(), "bad", "a.ogg", "audio/ogg", nil)
	if out.Class != capture.ClassPermanent {
		t.Errorf("invalid name = %+v", out)
	}
	out = c.Attach(ctx(), "memos/x/y", "a.ogg", "audio/ogg", nil)
	if out.Class != capture.ClassPermanent {
		t.Errorf("slash in name = %+v", out)
	}
	// Memo missing: 404 is a misconfiguration (retried), not a drop.
	out = c.Attach(ctx(), "memos/missing", "a.ogg", "audio/ogg", nil)
	if out.Class != capture.ClassMisconfig ||
		out.WaitingOn != capture.WaitingMemos {
		t.Errorf("missing memo = %+v", out)
	}

	f.memos["m1"] = map[string]any{}
	f.mu.Lock()
	f.prefix = "/api"
	f.mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/me"):
			_, _ = io.WriteString(w, `{"user":{"name":"users/1"}}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"memos/m1"}`)
		default:
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	t.Cleanup(srv.Close)
	c2 := NewClient(Config{BaseURL: srv.URL, Token: fakeToken, APIPrefix: "auto"}, srv.Client())
	if out := c2.Attach(ctx(), "memos/m1", "a.ogg", "audio/ogg", []byte("x")); out.Class != capture.ClassPermanent {
		t.Errorf("upload 413 = %+v", out)
	}
	bad := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not json") }),
	)
	t.Cleanup(bad.Close)
	c3 := NewClient(Config{BaseURL: bad.URL, Token: fakeToken, APIPrefix: "/api"}, bad.Client())
	if out := c3.Attach(ctx(), "memos/m1", "a.ogg", "audio/ogg", nil); out.Class != capture.ClassTemporary {
		t.Errorf("malformed memo = %+v", out)
	}
}

func TestResponseTooLarge(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Valid JSON whose padding pushes it one byte past the limit: it
		// must be reported as too large, never truncated and misparsed.
		pad := strings.Repeat("x", maxBody-len(`{"user":{"name":"users/1"},"p":""}`)+1)
		_, _ = io.WriteString(w, `{"user":{"name":"users/1"},"p":"`+pad+`"}`)
	}))
	t.Cleanup(big.Close)
	c := NewClient(Config{BaseURL: big.URL, Token: fakeToken, APIPrefix: "/api"}, big.Client())
	_, out := c.Owner(ctx())
	if out.Class != capture.ClassTemporary || out.Reason != "response too large" ||
		out.WaitingOn != capture.WaitingMemos {
		t.Errorf("oversized response = %+v", out)
	}

	fits := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pad := strings.Repeat("x", maxBody-len(`{"user":{"name":"users/1"},"p":""}`))
		_, _ = io.WriteString(w, `{"user":{"name":"users/1"},"p":"`+pad+`"}`)
	}))
	t.Cleanup(fits.Close)
	c = NewClient(Config{BaseURL: fits.URL, Token: fakeToken, APIPrefix: "/api"}, fits.Client())
	if owner, out := c.Owner(ctx()); out.Class != capture.ClassOK || owner != "users/1" {
		t.Errorf("response at the limit = %q %+v", owner, out)
	}
}

func TestProbeMalformed(t *testing.T) {
	bad := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not json") }),
	)
	t.Cleanup(bad.Close)
	c := NewClient(Config{BaseURL: bad.URL, Token: fakeToken}, bad.Client())
	if _, out := c.Owner(ctx()); out.Class != capture.ClassTemporary {
		t.Errorf("malformed probe = %+v", out)
	}
}

func TestCreateOtherErrorButMemoExists(t *testing.T) {
	uid := MemoID(3, receivedAt())
	dup := `{"code":13,"message":"duplicate key value violates unique constraint"}`
	f := newFake("/api")
	f.memos[uid] = map[string]any{}
	f.createStatus, f.createBody = 500, dup
	c := newClient(t, f, nil)
	name, link, out := c.Create(ctx(), Note{CaptureID: 3, ReceivedAt: receivedAt()})
	if out.Class != capture.ClassOK || name != "memos/"+uid || link != "https://memos.example.org/memos/"+uid {
		t.Fatalf("name=%q link=%q out=%+v", name, link, out)
	}
	if f.count("GET /api/memos/"+uid) != 1 {
		t.Errorf("existing memo not fetched: %v", f.calls)
	}
}

func TestCreateFailsAndMemoAbsentStaysTemporary(t *testing.T) {
	f := newFake("/api")
	f.createStatus, f.createBody = 500, `{"code":13,"message":"internal"}`
	c := newClient(t, f, nil)
	name, _, out := c.Create(ctx(), Note{CaptureID: 3, ReceivedAt: receivedAt()})
	if out.Class != capture.ClassTemporary || out.Reason != "http 500" || out.WaitingOn != capture.WaitingMemos ||
		name != "" {
		t.Fatalf("name=%q out=%+v", name, out)
	}
}

func TestProbeFallsThroughOnNonJSON2xx(t *testing.T) {
	for _, body := range []string{"<html>spa</html>", "{}", `{"user":{}}`} {
		f := newFake("/api/v1")
		f.legacyBody = body
		c := newClient(t, f, nil)
		if out := create(c, Note{CaptureID: 1, ReceivedAt: receivedAt()}); out.Class != capture.ClassOK {
			t.Fatalf("body %q: outcome = %+v", body, out)
		}
		if f.count("GET /api/auth/me") != 1 || f.count("GET /api/v1/auth/me") != 1 {
			t.Errorf("body %q: calls = %v", body, f.calls)
		}
	}
}

func TestNotFoundDropsAutoPrefix(t *testing.T) {
	f := newFake("/api/v1")
	c := newClient(t, f, nil)
	if out := create(c, Note{CaptureID: 1, ReceivedAt: receivedAt()}); out.Class != capture.ClassOK {
		t.Fatalf("create = %+v", out)
	}
	// 404 on the memo lookup: the outcome stays Misconfig, the cache is dropped.
	if out := c.Attach(ctx(), "memos/missing", "a.ogg", "audio/ogg", nil); out.Class != capture.ClassMisconfig {
		t.Fatalf("attach = %+v", out)
	}
	if out := create(c, Note{CaptureID: 2, ReceivedAt: receivedAt()}); out.Class != capture.ClassOK {
		t.Fatalf("create after 404 = %+v", out)
	}
	if got := f.count("GET /api/v1/auth/me"); got != 2 {
		t.Errorf("expected a re-probe, auth/me count = %d: %v", got, f.calls)
	}
}

func TestExplicitPrefixNeverReprobed(t *testing.T) {
	f := newFake("/api/v1")
	c := newClient(t, f, func(cfg *Config) { cfg.APIPrefix = "/api/v1" })
	if out := c.Attach(ctx(), "memos/missing", "a.ogg", "audio/ogg", nil); out.Class != capture.ClassMisconfig {
		t.Fatalf("attach = %+v", out)
	}
	if out := create(c, Note{CaptureID: 2, ReceivedAt: receivedAt()}); out.Class != capture.ClassOK {
		t.Fatalf("create = %+v", out)
	}
	if f.count("GET /api/v1/auth/me")+f.count("GET /api/auth/me") != 0 {
		t.Errorf("explicit prefix was probed: %v", f.calls)
	}
}

// TestCreateRetriedAfterTimeoutYieldsOneNote covers a create the server
// applied but whose response never reached the client: the retry must not
// produce a second note.
func TestCreateRetriedAfterTimeoutYieldsOneNote(t *testing.T) {
	f := newFake("/api/v1")
	release := make(chan struct{})
	var first sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slow := false
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/memos" {
			first.Do(func() { slow = true })
		}
		f.ServeHTTP(w, r) // stores the memo
		if slow {
			<-release // the response is lost: the client times out first
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	c := NewClient(Config{
		BaseURL: srv.URL, PublicURL: "https://memos.example.org/", Token: fakeToken,
		APIPrefix: "/api/v1", Version: "1.2.3",
	}, &http.Client{Timeout: 200 * time.Millisecond})
	n := Note{CaptureID: 9, Content: "x", ReceivedAt: receivedAt()}

	_, _, out := c.Create(ctx(), n)
	if out.Class != capture.ClassTemporary {
		t.Fatalf("timed-out create = %+v, want temporary", out)
	}
	name, _, out := c.Create(ctx(), n)
	uid := MemoID(9, receivedAt())
	if out.Class != capture.ClassOK || name != "memos/"+uid {
		t.Fatalf("retry: name=%q out=%+v", name, out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.memos) != 1 {
		t.Errorf("memos = %d, want 1", len(f.memos))
	}
}
