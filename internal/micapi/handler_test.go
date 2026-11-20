package micapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/micapi"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

const (
	apiToken = "FAKE-0123456789abcdef0123456789abcdef"
	maxBytes = 1 << 20
)

func serverNow() time.Time { return time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC) }

type ingestCall struct {
	meta pipeline.RecordingMeta
	path string
	data []byte
}

type fakeIngester struct {
	mu     sync.Mutex
	dir    string
	calls  []ingestCall
	result pipeline.RecordingResult
	err    error
}

func (f *fakeIngester) IngestRecording(
	_ context.Context, meta pipeline.RecordingMeta, localPath string,
) (pipeline.RecordingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, _ := os.ReadFile(filepath.Join(f.dir, localPath))
	f.calls = append(f.calls, ingestCall{meta, localPath, data})
	if f.err != nil {
		return 0, f.err
	}
	if f.result == 0 {
		return pipeline.Stored, nil
	}

	return f.result, nil
}

type noticeCall struct {
	id       int64
	at       time.Time
	deadline bool
	ctxErr   error
}

type fakeNotices struct {
	started   chan noticeCall
	discarded chan noticeCall
	err       error
	block     chan struct{}
}

func newFakeNotices() *fakeNotices {
	return &fakeNotices{started: make(chan noticeCall, 4), discarded: make(chan noticeCall, 4)}
}

func (f *fakeNotices) Started(ctx context.Context, id int64, at time.Time) error {
	if f.block != nil {
		<-f.block
	}

	_, dl := ctx.Deadline()
	f.started <- noticeCall{id, at, dl, ctx.Err()}

	return f.err
}

func (f *fakeNotices) Discarded(ctx context.Context, id int64) error {
	_, dl := ctx.Deadline()
	f.discarded <- noticeCall{id: id, deadline: dl, ctxErr: ctx.Err()}

	return f.err
}

type upsert struct {
	name                  string
	lastSeen              time.Time
	receiver, storageFull bool
}

type fakeRecorders struct {
	mu      sync.Mutex
	upserts []upsert
	err     error
}

func (f *fakeRecorders) UpsertRecorder(
	_ context.Context, name string, lastSeen time.Time, receiverPresent, storageFull bool,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.upserts = append(f.upserts, upsert{name, lastSeen, receiverPresent, storageFull})

	return nil
}

type env struct {
	h       *micapi.Handler
	dir     string
	ing     *fakeIngester
	notices *fakeNotices
	recs    *fakeRecorders
	logs    *observer.ObservedLogs
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "files")
	core, logs := observer.New(zap.DebugLevel)
	e := &env{
		dir: dir, ing: &fakeIngester{dir: dir}, notices: newFakeNotices(), recs: &fakeRecorders{}, logs: logs,
	}
	e.h = micapi.NewHandler(
		micapi.Config{Token: apiToken, MaxBytes: maxBytes, FilesDir: dir},
		e.ing, e.notices, e.recs,
		micapi.WithClock(serverNow),
		micapi.WithLogger(zap.New(core)),
	)
	t.Cleanup(e.h.Wait)

	return e
}

func wav(samples int) []byte {
	return append(mic.Header(int64(samples*2)), make([]byte, samples*2)...)
}

func sum(b []byte) string {
	s := sha256.Sum256(b)

	return hex.EncodeToString(s[:])
}

func uploadReq(id string, body []byte) *http.Request {
	r := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		"/mic/v1/recordings/"+id,
		bytes.NewReader(body),
	)
	r.Header.Set("Authorization", "Bearer "+apiToken)
	r.Header.Set("Content-Type", "audio/wav")
	r.Header.Set("X-Content-Sha256", sum(body))
	r.Header.Set("X-Recording-Started-At", "2026-11-20T06:30:01.123456789Z")
	r.Header.Set("X-Recording-End-Reason", "max_length")
	r.Header.Set("X-Recorder", "bedside")

	return r
}

type reply struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func (e *env) do(t *testing.T, r *http.Request) (*httptest.ResponseRecorder, reply) {
	t.Helper()
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	var rep reply
	if w.Body.Len() > 0 {
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q, body %q", ct, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatalf("body %q: %v", w.Body.String(), err)
		}
	}

	return w, rep
}

func (e *env) files(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(e.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range ents {
		out = append(out, ent.Name())
	}

	return out
}

func TestUploadStored(t *testing.T) {
	e := newEnv(t)
	body := wav(1600)
	w, rep := e.do(t, uploadReq("42", body))
	if w.Code != http.StatusCreated || rep.Status != "stored" {
		t.Fatalf("got %d %+v", w.Code, rep)
	}
	if len(e.ing.calls) != 1 {
		t.Fatalf("ingest calls = %d", len(e.ing.calls))
	}
	c := e.ing.calls[0]
	want := pipeline.RecordingMeta{
		ID: 42, StartedAt: time.Date(2026, 11, 20, 6, 30, 1, 123456789, time.UTC),
		EndReason: "max_length", Recorder: "bedside",
	}
	if c.meta != want {
		t.Fatalf("meta = %+v", c.meta)
	}
	if !bytes.Equal(c.data, body) {
		t.Fatal("ingested file does not hold the body")
	}
	if strings.Contains(c.path, "/") || !strings.HasPrefix(c.path, "mic-42-") || !strings.HasSuffix(c.path, ".wav") {
		t.Fatalf("path = %q", c.path)
	}
	if got := e.files(t); len(got) != 1 || got[0] != c.path {
		t.Fatalf("files = %v", got)
	}
}

func TestUploadNamesAreUnique(t *testing.T) {
	e := newEnv(t)
	body := wav(10)
	e.do(t, uploadReq("42", body))
	e.do(t, uploadReq("42", body))
	if len(e.ing.calls) != 2 || e.ing.calls[0].path == e.ing.calls[1].path {
		t.Fatalf("calls = %+v", e.ing.calls)
	}
}

func TestUploadDuplicateRemovesFile(t *testing.T) {
	e := newEnv(t)
	e.ing.result = pipeline.Duplicate
	w, rep := e.do(t, uploadReq("42", wav(10)))
	if w.Code != http.StatusOK || rep.Status != "duplicate" {
		t.Fatalf("got %d %+v", w.Code, rep)
	}
	if got := e.files(t); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
}

func TestUploadIngestFailure(t *testing.T) {
	e := newEnv(t)
	e.ing.err = errors.New("database is locked")
	w, rep := e.do(t, uploadReq("42", wav(10)))
	if w.Code != http.StatusServiceUnavailable || rep.Status != "unavailable" {
		t.Fatalf("got %d %+v", w.Code, rep)
	}
	if strings.Contains(rep.Reason, "locked") {
		t.Fatalf("reason leaks the error: %q", rep.Reason)
	}
	if got := e.files(t); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
}

func TestUploadDiskFailure(t *testing.T) {
	e := newEnv(t)
	// A regular file where the files directory should be.
	if err := os.WriteFile(e.dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w, rep := e.do(t, uploadReq("42", wav(10)))
	if w.Code != http.StatusServiceUnavailable || rep.Status != "unavailable" {
		t.Fatalf("got %d %+v", w.Code, rep)
	}
	if len(e.ing.calls) != 0 {
		t.Fatal("ingested despite the disk failure")
	}
}

func TestUploadCorrupt(t *testing.T) {
	tests := []struct {
		name string
		req  func() *http.Request
	}{
		{"hash mismatch", func() *http.Request {
			r := uploadReq("42", wav(10))
			r.Header.Set("X-Content-Sha256", sum([]byte("other")))

			return r
		}},
		{"shorter than declared", func() *http.Request {
			r := uploadReq("42", wav(10))
			r.ContentLength += 5

			return r
		}},
		{"longer than declared", func() *http.Request {
			r := uploadReq("42", wav(10))
			r.ContentLength -= 5

			return r
		}},
		{"truncated body", func() *http.Request {
			body := wav(10)
			r := uploadReq("42", body)
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body[:20]), errReader{}))

			return r
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			w, rep := e.do(t, tt.req())
			if w.Code != http.StatusBadRequest || rep.Status != "corrupt" {
				t.Fatalf("got %d %+v", w.Code, rep)
			}
			if got := e.files(t); len(got) != 0 {
				t.Fatalf("temp file left: %v", got)
			}
			if len(e.ing.calls) != 0 {
				t.Fatal("ingested a corrupt upload")
			}
		})
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestUploadUnsupportedWAV(t *testing.T) {
	e := newEnv(t)
	body := wav(10)
	body[24] = 0x44 // sample rate 44100-ish instead of 16000
	body[25] = 0xAC
	w, rep := e.do(t, uploadReq("42", body))
	if w.Code != http.StatusUnsupportedMediaType || rep.Status != "unsupported" {
		t.Fatalf("got %d %+v", w.Code, rep)
	}
	if got := e.files(t); len(got) != 0 {
		t.Fatalf("temp file left: %v", got)
	}
}

func TestUploadBodyOverLimit(t *testing.T) {
	e := newEnv(t)
	body := make([]byte, maxBytes+10)
	r := uploadReq("42", body)
	r.ContentLength = 100 // lies; the body reader still enforces the limit
	w, rep := e.do(t, r)
	if w.Code != http.StatusRequestEntityTooLarge || rep.Status != "too_large" {
		t.Fatalf("got %d %+v", w.Code, rep)
	}
	if got := e.files(t); len(got) != 0 {
		t.Fatalf("temp file left: %v", got)
	}
}

type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) { c.n += len(p); return len(p), nil }

func TestUploadHeaderRejections(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		edit   func(*http.Request)
		code   int
		status string
	}{
		{"bad id", "abc", func(*http.Request) {}, 400, "bad_request"},
		{"zero id", "0", func(*http.Request) {}, 400, "bad_request"},
		{"content type", "42", func(r *http.Request) { r.Header.Set("Content-Type", "audio/ogg") }, 415, "unsupported"},
		{"too large", "42", func(r *http.Request) { r.ContentLength = maxBytes + 1 }, 413, "too_large"},
		{"no length", "42", func(r *http.Request) { r.ContentLength = -1 }, 400, "bad_request"},
		{"no hash", "42", func(r *http.Request) { r.Header.Del("X-Content-Sha256") }, 400, "bad_request"},
		{
			"bad started",
			"42",
			func(r *http.Request) { r.Header.Set("X-Recording-Started-At", "x") },
			400,
			"bad_request",
		},
		{"bad reason", "42", func(r *http.Request) { r.Header.Set("X-Recording-End-Reason", "x") }, 400, "bad_request"},
		{"bad recorder", "42", func(r *http.Request) { r.Header.Set("X-Recorder", "") }, 400, "bad_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			cr := &countingReader{}
			r := uploadReq(tt.id, nil)
			r.Body = io.NopCloser(cr)
			r.ContentLength = 100
			tt.edit(r)
			w, rep := e.do(t, r)
			if w.Code != tt.code || rep.Status != tt.status || rep.Reason == "" {
				t.Fatalf("got %d %+v", w.Code, rep)
			}
			if cr.n != 0 {
				t.Fatalf("read %d body bytes before rejecting", cr.n)
			}
			if got := e.files(t); len(got) != 0 {
				t.Fatalf("files = %v", got)
			}
		})
	}
}

func TestUnauthorized(t *testing.T) {
	reqs := []func() *http.Request{
		func() *http.Request { return uploadReq("42", wav(10)) },
		func() *http.Request {
			return httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPost,
				"/mic/v1/recordings/42/started",
				strings.NewReader("{}"),
			)
		},
		func() *http.Request {
			return httptest.NewRequestWithContext(
				context.Background(),
				http.MethodDelete,
				"/mic/v1/recordings/42/started",
				http.NoBody,
			)
		},
		func() *http.Request {
			return httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPut,
				"/mic/v1/recorders/bedside",
				strings.NewReader("{}"),
			)
		},
		func() *http.Request {
			return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mic/v1/nowhere", http.NoBody)
		},
	}
	for _, auth := range []string{"", "Bearer FAKE-wrong-token", "Basic " + apiToken} {
		for i, mk := range reqs {
			t.Run(auth+"/"+strconv.Itoa(i), func(t *testing.T) {
				e := newEnv(t)
				r := mk()
				if auth == "" {
					r.Header.Del("Authorization")
				} else {
					r.Header.Set("Authorization", auth)
				}
				w, rep := e.do(t, r)
				if w.Code != http.StatusUnauthorized || rep.Status != "unauthorized" {
					t.Fatalf("got %d %+v", w.Code, rep)
				}
				if strings.Contains(w.Body.String(), "FAKE") {
					t.Fatalf("body leaks a token: %q", w.Body.String())
				}
				if len(e.ing.calls) != 0 || len(e.recs.upserts) != 0 || len(e.files(t)) != 0 {
					t.Fatal("stored something for an unauthorized request")
				}
				assertUnauthorizedLog(t, e.logs, r.URL.Path)
			})
		}
	}
}

func assertUnauthorizedLog(t *testing.T, logs *observer.ObservedLogs, path string) {
	t.Helper()
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if len(fields) != 2 || fields["path"] != path || fields["status"] != int64(http.StatusUnauthorized) {
		t.Fatalf("log fields = %v", fields)
	}
	if strings.Contains(entries[0].Message, "FAKE") {
		t.Fatalf("log message leaks: %q", entries[0].Message)
	}
}

func TestEmptyTokenRejectsEverything(t *testing.T) {
	ing := &fakeIngester{}
	h := micapi.NewHandler(micapi.Config{MaxBytes: maxBytes, FilesDir: t.TempDir()},
		ing, newFakeNotices(), &fakeRecorders{})
	r := uploadReq("42", wav(10))
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", w.Code)
	}
}

func authed(method, target, body string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+apiToken)

	return r
}

func TestStartedAccepted(t *testing.T) {
	e := newEnv(t)
	w, rep := e.do(t, authed(http.MethodPost, "/mic/v1/recordings/42/started",
		`{"started_at":"2026-11-20T06:30:01.5Z","recorder":"bedside"}`))
	if w.Code != http.StatusAccepted || rep.Status != "accepted" {
		t.Fatalf("got %d %+v", w.Code, rep)
	}
	select {
	case c := <-e.notices.started:
		if c.id != 42 || c.at != time.Date(2026, 11, 20, 6, 30, 1, 500000000, time.UTC) {
			t.Fatalf("call = %+v", c)
		}
		if !c.deadline || c.ctxErr != nil {
			t.Fatalf("context: deadline %v, err %v", c.deadline, c.ctxErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Started was not called")
	}
}

func TestStartedRepliesBeforeTheSend(t *testing.T) {
	e := newEnv(t)
	e.notices.block = make(chan struct{})
	e.notices.err = errors.New("telegram down")
	ctx, cancel := context.WithCancel(context.Background())
	r := authed(http.MethodPost, "/mic/v1/recordings/42/started",
		`{"started_at":"2026-11-20T06:30:01Z","recorder":"bedside"}`).WithContext(ctx)
	w, _ := e.do(t, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d", w.Code)
	}
	cancel() // the request is over; the send must not be cancelled with it
	close(e.notices.block)
	if c := <-e.notices.started; c.ctxErr != nil {
		t.Fatalf("context error = %v", c.ctxErr)
	}
	e.h.Wait()
}

func TestStartedBadRequests(t *testing.T) {
	tests := []struct{ name, target, body string }{
		{"bad id", "/mic/v1/recordings/-1/started", `{"started_at":"2026-11-20T06:30:01Z","recorder":"bedside"}`},
		{"bad json", "/mic/v1/recordings/42/started", `{`},
		{"bad recorder", "/mic/v1/recordings/42/started", `{"started_at":"2026-11-20T06:30:01Z","recorder":"a b"}`},
		{"huge body", "/mic/v1/recordings/42/started", `{"recorder":"` + strings.Repeat("a", 10000) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			w, rep := e.do(t, authed(http.MethodPost, tt.target, tt.body))
			if w.Code != http.StatusBadRequest || rep.Status != "bad_request" {
				t.Fatalf("got %d %+v", w.Code, rep)
			}
			e.h.Wait()
			if len(e.notices.started) != 0 {
				t.Fatal("Started called for a bad request")
			}
		})
	}
}

func TestDiscardNotice(t *testing.T) {
	for _, fail := range []bool{false, true} {
		e := newEnv(t)
		if fail {
			e.notices.err = errors.New("telegram down")
		}
		w, _ := e.do(t, authed(http.MethodDelete, "/mic/v1/recordings/42/started", ""))
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
			t.Fatalf("fail=%v: got %d %q", fail, w.Code, w.Body.String())
		}
		c := <-e.notices.discarded
		if c.id != 42 || !c.deadline {
			t.Fatalf("call = %+v", c)
		}
	}
}

func TestDiscardNoticeBadID(t *testing.T) {
	e := newEnv(t)
	w, rep := e.do(t, authed(http.MethodDelete, "/mic/v1/recordings/x/started", ""))
	if w.Code != http.StatusBadRequest || rep.Status != "bad_request" {
		t.Fatalf("got %d %+v", w.Code, rep)
	}
	if len(e.notices.discarded) != 0 {
		t.Fatal("Discarded called for a bad ID")
	}
}

func TestCheckIn(t *testing.T) {
	e := newEnv(t)
	w, _ := e.do(t, authed(http.MethodPut, "/mic/v1/recorders/bedside",
		`{"receiver_present":true,"storage_full":false,"pending":0,"oldest_pending_at":null}`))
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	want := []upsert{{"bedside", serverNow(), true, false}}
	if len(e.recs.upserts) != 1 || e.recs.upserts[0] != want[0] {
		t.Fatalf("upserts = %+v", e.recs.upserts)
	}
}

func TestCheckInRejections(t *testing.T) {
	tests := []struct {
		name, target, body string
		storeErr           error
		code               int
		status             string
	}{
		{
			"bad name",
			"/mic/v1/recorders/a%20b",
			`{"receiver_present":true,"storage_full":false}`,
			nil,
			400,
			"bad_request",
		},
		{
			"long name",
			"/mic/v1/recorders/" + strings.Repeat("a", 65),
			`{"receiver_present":true,"storage_full":false}`,
			nil,
			400,
			"bad_request",
		},
		{"bad body", "/mic/v1/recorders/bedside", `nope`, nil, 400, "bad_request"},
		{
			"store down", "/mic/v1/recorders/bedside", `{"receiver_present":true,"storage_full":false}`,
			errors.New("disk I/O error"), 503, "unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.recs.err = tt.storeErr
			w, rep := e.do(t, authed(http.MethodPut, tt.target, tt.body))
			if w.Code != tt.code || rep.Status != tt.status {
				t.Fatalf("got %d %+v", w.Code, rep)
			}
			if len(e.recs.upserts) != 0 {
				t.Fatalf("upserts = %+v", e.recs.upserts)
			}
		})
	}
}

func TestUnknownRoute(t *testing.T) {
	e := newEnv(t)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, authed(http.MethodGet, "/mic/v1/nowhere", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d", w.Code)
	}
}

// TestOverHTTP drives the handler through a real server and client, so that
// Content-Length and the body reader behave as in production.
func TestOverHTTP(t *testing.T) {
	e := newEnv(t)
	mux := http.NewServeMux()
	mux.Handle("/mic/v1/", e.h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := wav(1600)
	r := uploadReq("7", body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut,
		srv.URL+"/mic/v1/recordings/7", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = r.Header
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("code = %d", resp.StatusCode)
	}
	if len(e.ing.calls) != 1 || !bytes.Equal(e.ing.calls[0].data, body) {
		t.Fatal("upload not ingested intact")
	}
}
