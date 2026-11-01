package docling

// The wire shapes below are pinned against docling-serve v1.38.2
// and the options model it imports from docling (datamodel/service/options.py,
// ConvertDocumentsOptions; responses.py beside it):
//
//   - POST /v1/convert/file/async takes multipart/form-data: the file part is
//     "files" (list[UploadFile]) and every ConvertDocumentsOptions field is a
//     form field of the same name (docling_serve/helper_functions.py,
//     FormDepends). The response is TaskStatusResponse.
//   - page_range is PageRange = Tuple[int, int]. In /openapi.json the form
//     field is {"type":"array","prefixItems":[{"type":"integer"},
//     {"type":"integer"}],"minItems":2,"maxItems":2}. FastAPI reads a tuple
//     form field with FormData.getlist (fastapi/dependencies/utils.py,
//     _get_multidict_value; tuple is in fastapi/_compat/shared.py
//     sequence_types), so it is the same field sent twice: page_range=1,
//     page_range=20. Checked against FastAPI 0.162.0: the repeated field
//     parses to (1, 20), while "1,20" or "[1,20]" in one field is a 422.
//   - document_timeout is Optional[float], in seconds.
//   - GET /v1/status/poll/{task_id} returns TaskStatusResponse:
//     {"task_id","task_type","task_status","task_position",...}, where
//     task_status is a ConversionStatus: pending, started, success,
//     partial_success, failure, skipped. An unknown task is a 404.
//   - GET /v1/result/{task_id} returns, for one file converted to md,
//     ConvertDocumentResponse {"document":{"filename","md_content",...},
//     "status","errors","processing_time"}, or TaskFailureResult
//     {"kind":"TaskFailureResult","failure":{...}} for a failed task.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

const (
	taskID     = "task-0001"
	submitBody = `{"task_id":"task-0001","task_type":"convert","task_status":"pending","task_position":1}`
	resultOK   = `{"document":{"filename":"bill.pdf","md_content":"# Bill\n\nTotal 42"},` +
		`"status":"success","errors":[],"processing_time":1.5}`
)

func pollBody(status string) string {
	return `{"task_id":"task-0001","task_type":"convert","task_status":"` + status + `","task_position":0}`
}

// fakeClock advances only when the client sleeps.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.t
}

func (f *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
	f.sleeps = append(f.sleeps, d)

	return nil
}

// fakeServer answers submit, poll and result with scripted replies.
type fakeServer struct {
	mu        sync.Mutex
	submit    func(w http.ResponseWriter, r *http.Request)
	polls     []string // task_status per poll; the last one repeats
	pollCode  int
	result    string
	resultHit int
	calls     []string
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/convert/file/async":
		if s.submit != nil {
			s.submit(w, r)
			return
		}
		_, _ = io.WriteString(w, submitBody)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/status/poll/"+taskID:
		if s.pollCode != 0 {
			w.WriteHeader(s.pollCode)
			return
		}
		status := s.polls[0]
		if len(s.polls) > 1 {
			s.polls = s.polls[1:]
		}
		if strings.HasPrefix(status, "{") || status == "garbage" {
			_, _ = io.WriteString(w, status)
			return
		}
		_, _ = io.WriteString(w, pollBody(status))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/result/"+taskID:
		s.resultHit++
		if code, ok := strings.CutPrefix(s.result, "HTTP "); ok {
			switch code {
			case "404":
				w.WriteHeader(http.StatusNotFound)
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
			}

			return
		}
		_, _ = io.WriteString(w, s.result)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func newTestClient(t *testing.T, srv *fakeServer, mutate func(*Config)) (*Client, *fakeClock) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	cfg := Config{
		BaseURL:  ts.URL + "/",
		APIKey:   "FAKE-DOCLING-KEY",
		MaxPages: 20,
		Timeout:  10 * time.Minute,
		Version:  "1.2.3",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c := NewClient(cfg, ts.Client())
	clk := &fakeClock{t: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)}
	c.now, c.sleep = clk.now, clk.sleep

	return c, clk
}

func TestConvertSubmitsPinnedForm(t *testing.T) {
	data := []byte("%PDF-1.7 binary\xff\x00")
	var got struct {
		fields      map[string][]string
		fileName    string
		fileType    string
		file        []byte
		key, ua, ct string
	}
	srv := &fakeServer{polls: []string{"pending", "started", "success"}, result: resultOK}
	srv.submit = func(w http.ResponseWriter, r *http.Request) {
		got.key, got.ua, got.ct = r.Header.Get("X-Api-Key"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		got.fields = r.MultipartForm.Value
		fhs := r.MultipartForm.File["files"]
		if len(fhs) != 1 {
			t.Errorf("files parts = %d, want 1", len(fhs))
			return
		}
		got.fileName, got.fileType = fhs[0].Filename, fhs[0].Header.Get("Content-Type")
		f, _ := fhs[0].Open()
		got.file, _ = io.ReadAll(f)
		_, _ = io.WriteString(w, submitBody)
	}
	c, clk := newTestClient(t, srv, nil)

	res, out := c.Convert(context.Background(), "bill.pdf", "application/pdf", data)

	if out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v, want OK", out)
	}
	if res.Markdown != "# Bill\n\nTotal 42" || res.TimedOut {
		t.Errorf("result = %+v", res)
	}
	want := map[string][]string{
		"to_formats":        {"md"},
		"image_export_mode": {"placeholder"},
		"include_images":    {"false"},
		"do_ocr":            {"true"},
		"abort_on_error":    {"false"},
		"page_range":        {"1", "20"},
		"document_timeout":  {"600"},
	}
	if len(got.fields) != len(want) {
		t.Errorf("fields = %v, want %v", got.fields, want)
	}
	for k, v := range want {
		if strings.Join(got.fields[k], ",") != strings.Join(v, ",") {
			t.Errorf("field %s = %q, want %q", k, got.fields[k], v)
		}
	}
	if got.fileName != "bill.pdf" || got.fileType != "application/pdf" || !bytes.Equal(got.file, data) {
		t.Errorf("file part = %q %q %q", got.fileName, got.fileType, got.file)
	}
	if got.key != "FAKE-DOCLING-KEY" || got.ua != "VoiceNotes/1.2.3" ||
		!strings.HasPrefix(got.ct, "multipart/form-data; boundary=") {
		t.Errorf("headers: key=%q ua=%q ct=%q", got.key, got.ua, got.ct)
	}
	wantCalls := []string{
		"POST /v1/convert/file/async",
		"GET /v1/status/poll/" + taskID,
		"GET /v1/status/poll/" + taskID,
		"GET /v1/status/poll/" + taskID,
		"GET /v1/result/" + taskID,
	}
	if strings.Join(srv.calls, "|") != strings.Join(wantCalls, "|") {
		t.Errorf("calls = %v, want %v", srv.calls, wantCalls)
	}
	if len(clk.sleeps) != 2 || clk.sleeps[0] != 2*time.Second || clk.sleeps[1] != 2*time.Second {
		t.Errorf("sleeps = %v, want two of 2s", clk.sleeps)
	}
}

// TestConvertSendsNoOCROptions pins that no OCR option is sent, so the
// server's own OCR preset decides (contracts/external-apis.md section 1).
func TestConvertSendsNoOCROptions(t *testing.T) {
	var fields map[string][]string
	srv := &fakeServer{polls: []string{"success"}, result: resultOK}
	srv.submit = func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		fields = r.MultipartForm.Value
		_, _ = io.WriteString(w, submitBody)
	}
	c, _ := newTestClient(t, srv, nil)

	_, out := c.Convert(context.Background(), "scan.pdf", "application/pdf", []byte("x"))
	if out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v, want OK", out)
	}
	for _, k := range []string{"ocr_engine", "ocr_preset", "ocr_lang", "force_ocr"} {
		if v, ok := fields[k]; ok {
			t.Errorf("field %s = %q was sent, want absent", k, v)
		}
	}
}

func TestConvertFilePartDefaults(t *testing.T) {
	var name, ctype, key string
	var keySet bool
	srv := &fakeServer{polls: []string{"success"}, result: resultOK}
	srv.submit = func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("X-Api-Key")
		_, keySet = r.Header["X-Api-Key"]
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		fh := r.MultipartForm.File["files"][0]
		name, ctype = fh.Filename, fh.Header.Get("Content-Type")
		_, _ = io.WriteString(w, submitBody)
	}
	c, _ := newTestClient(t, srv, func(cfg *Config) { cfg.APIKey = "" })

	_, out := c.Convert(context.Background(), `my "odd" file.docx`, "", []byte("x"))

	if out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v", out)
	}
	if keySet || key != "" {
		t.Errorf("X-Api-Key sent without a key: %q", key)
	}
	if name != `my "odd" file.docx` || ctype != "application/octet-stream" {
		t.Errorf("file part = %q %q", name, ctype)
	}
}

func TestConvertResultStatus(t *testing.T) {
	cases := []struct {
		name      string
		result    string
		wantClass capture.Class
		wantText  string
		wantWhy   string
	}{
		{"success", resultOK, capture.ClassOK, "# Bill\n\nTotal 42", ""},
		{
			"partial success keeps text",
			`{"document":{"filename":"a.pdf","md_content":"page one"},"status":"partial_success","errors":[{}],"processing_time":1}`,
			capture.ClassOK,
			"page one",
			"",
		},
		{
			"empty text",
			`{"document":{"filename":"a.pdf","md_content":null},"status":"success","errors":[],"processing_time":1}`,
			capture.ClassOK, "", "",
		},
		{
			"failure",
			`{"document":{"filename":"a.pdf"},"status":"failure","errors":[],"processing_time":1}`,
			capture.ClassPermanent, "", ReasonUnreadable,
		},
		{
			"skipped",
			`{"document":{"filename":"a.pdf"},"status":"skipped","errors":[],"processing_time":1}`,
			capture.ClassPermanent, "", ReasonUnreadable,
		},
		{
			"task failure result",
			`{"kind":"TaskFailureResult","failure":{"code":"x","message":"m","phase":"convert","details":{}}}`,
			capture.ClassPermanent, "", ReasonUnreadable,
		},
		{"malformed json", `{"document":`, capture.ClassTemporary, "", "malformed response"},
		{"no status", `{"document":{"md_content":"x"}}`, capture.ClassTemporary, "", "malformed response"},
		{"result 503", "HTTP 503", capture.ClassTemporary, "", "http 503"},
		{"result 404 is a lost task", "HTTP 404", capture.ClassTemporary, "", "task lost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fakeServer{polls: []string{"success"}, result: tc.result}
			c, _ := newTestClient(t, srv, nil)

			res, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

			if out.Class != tc.wantClass || out.Reason != tc.wantWhy {
				t.Errorf("outcome = %+v, want %v %q", out, tc.wantClass, tc.wantWhy)
			}
			if tc.wantClass != capture.ClassOK && out.WaitingOn != capture.WaitingDocling {
				t.Errorf("waiting on = %q, want docling", out.WaitingOn)
			}
			if res.Markdown != tc.wantText || res.TimedOut {
				t.Errorf("result = %+v, want text %q", res, tc.wantText)
			}
		})
	}
}

func TestConvertPollFailureStops(t *testing.T) {
	for _, status := range []string{"failure", "skipped"} {
		t.Run(status, func(t *testing.T) {
			srv := &fakeServer{polls: []string{"started", status}, result: resultOK}
			c, _ := newTestClient(t, srv, nil)

			res, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

			if out.Class != capture.ClassPermanent || out.Reason != ReasonUnreadable ||
				out.WaitingOn != capture.WaitingDocling {
				t.Errorf("outcome = %+v", out)
			}
			if res.TimedOut || srv.resultHit != 0 {
				t.Errorf("result fetched after poll failure: %+v, hits %d", res, srv.resultHit)
			}
		})
	}
}

func TestConvertPollPartialSuccessFetches(t *testing.T) {
	srv := &fakeServer{polls: []string{"partial_success"}, result: resultOK}
	c, _ := newTestClient(t, srv, nil)

	res, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

	if out.Class != capture.ClassOK || res.Markdown == "" || srv.resultHit != 1 {
		t.Errorf("outcome = %+v, result = %+v, hits %d", out, res, srv.resultHit)
	}
}

func TestConvertSubmitHTTPClassification(t *testing.T) {
	cases := []struct {
		code      int
		wantClass capture.Class
		wantWhy   string
	}{
		{http.StatusBadRequest, capture.ClassPermanent, ReasonRejected},
		{http.StatusRequestEntityTooLarge, capture.ClassPermanent, ReasonRejected},
		{http.StatusUnsupportedMediaType, capture.ClassPermanent, ReasonUnsupported},
		{http.StatusUnprocessableEntity, capture.ClassPermanent, ReasonRejected},
		{http.StatusUnauthorized, capture.ClassMisconfig, "http 401"},
		{http.StatusForbidden, capture.ClassMisconfig, "http 403"},
		{http.StatusNotFound, capture.ClassMisconfig, "http 404"},
		{http.StatusRequestTimeout, capture.ClassTemporary, "http 408"},
		{http.StatusTooManyRequests, capture.ClassTemporary, "http 429"},
		{http.StatusInternalServerError, capture.ClassTemporary, "http 500"},
		{http.StatusServiceUnavailable, capture.ClassTemporary, "http 503"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			srv := &fakeServer{submit: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, "SECRET-BODY FAKE-DOCLING-KEY")
			}}
			c, _ := newTestClient(t, srv, nil)

			res, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

			if out.Class != tc.wantClass || out.Reason != tc.wantWhy || out.WaitingOn != capture.WaitingDocling {
				t.Errorf("outcome = %+v, want %v %q docling", out, tc.wantClass, tc.wantWhy)
			}
			if res.TimedOut || len(srv.calls) != 1 {
				t.Errorf("result = %+v, calls = %v", res, srv.calls)
			}
		})
	}
}

func TestConvertPollHTTPClassification(t *testing.T) {
	cases := []struct {
		code      int
		wantClass capture.Class
		wantWhy   string
	}{
		{http.StatusNotFound, capture.ClassTemporary, "task lost"}, // resubmit on retry
		{http.StatusUnauthorized, capture.ClassMisconfig, "http 401"},
		{http.StatusBadGateway, capture.ClassTemporary, "http 502"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			srv := &fakeServer{pollCode: tc.code}
			c, _ := newTestClient(t, srv, nil)

			_, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

			if out.Class != tc.wantClass || out.Reason != tc.wantWhy || out.WaitingOn != capture.WaitingDocling {
				t.Errorf("outcome = %+v, want %v %q docling", out, tc.wantClass, tc.wantWhy)
			}
		})
	}
}

func TestConvertMalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		srv  *fakeServer
	}{
		{"submit garbage", &fakeServer{submit: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html>")
		}}},
		{"submit without task id", &fakeServer{submit: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"task_status":"pending"}`)
		}}},
		{"poll garbage", &fakeServer{polls: []string{"garbage"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.srv, nil)

			res, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

			if out.Class != capture.ClassTemporary || out.Reason != "malformed response" ||
				out.WaitingOn != capture.WaitingDocling || res.TimedOut {
				t.Errorf("outcome = %+v, result = %+v", out, res)
			}
		})
	}
}

func TestConvertNetworkError(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	base := ts.URL
	ts.Close()
	c := NewClient(Config{BaseURL: base, MaxPages: 20, Timeout: time.Minute}, nil)

	_, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

	if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingDocling {
		t.Errorf("outcome = %+v", out)
	}
}

func TestConvertTimesOut(t *testing.T) {
	srv := &fakeServer{polls: []string{"pending", "started"}, result: resultOK}
	c, clk := newTestClient(t, srv, func(cfg *Config) { cfg.Timeout = 5 * time.Second })

	res, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

	if !res.TimedOut || res.Markdown != "" {
		t.Errorf("result = %+v, want timed out", res)
	}
	if out.Class != capture.ClassTemporary || out.Reason != "timed out" || out.WaitingOn != capture.WaitingDocling {
		t.Errorf("outcome = %+v", out)
	}
	if srv.resultHit != 0 {
		t.Error("result fetched after a timeout")
	}
	var waited time.Duration
	for _, d := range clk.sleeps {
		waited += d
	}
	if waited != 5*time.Second {
		t.Errorf("waited %v (%v), want exactly the 5s timeout", waited, clk.sleeps)
	}
}

func TestConvertContextCancelled(t *testing.T) {
	srv := &fakeServer{polls: []string{"pending"}}
	c, _ := newTestClient(t, srv, nil)
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	res, out := c.Convert(ctx, "a.pdf", "application/pdf", []byte("x"))

	if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingDocling || res.TimedOut {
		t.Errorf("outcome = %+v, result = %+v", out, res)
	}
}

func TestConvertResultBodyIsBounded(t *testing.T) {
	big := `{"document":{"filename":"a.pdf","md_content":"` + strings.Repeat("a", 4096) +
		`"},"status":"success","errors":[],"processing_time":1}`
	srv := &fakeServer{polls: []string{"success"}, result: big}
	c, _ := newTestClient(t, srv, nil)
	c.resultLimit = 1024

	res, out := c.Convert(context.Background(), "a.pdf", "application/pdf", []byte("x"))

	if out.Class != capture.ClassTemporary || out.Reason != "malformed response" || res.Markdown != "" {
		t.Errorf("outcome = %+v, text length %d", out, len(res.Markdown))
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://docling.example.org:5001", MaxPages: 20, Timeout: time.Minute}, nil)
	if c.client == nil || c.client.Timeout <= 0 {
		t.Error("default http client missing a timeout")
	}
	if c.interval != 2*time.Second {
		t.Errorf("poll interval = %v, want 2s", c.interval)
	}
	if c.resultLimit != 32<<20 {
		t.Errorf("result limit = %d, want 32 MiB", c.resultLimit)
	}
	if c.now == nil || c.sleep == nil {
		t.Error("clock hooks not set")
	}
}
