package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/digest"
	"github.com/Toshik1978/voicenotes/internal/health"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
)

func TestHealthURL(t *testing.T) {
	tests := []struct {
		addr, want string
		wantErr    bool
	}{
		{":8080", "http://127.0.0.1:8080/healthz", false},
		{"", "http://127.0.0.1:8080/healthz", false},
		{"0.0.0.0:9090", "http://127.0.0.1:9090/healthz", false},
		{"[::]:9090", "http://127.0.0.1:9090/healthz", false},
		{"nonsense", "", true},
		{":", "", true},
	}
	for _, tt := range tests {
		got, err := healthURL(tt.addr)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("healthURL(%q) = %q, %v; want %q (err %v)", tt.addr, got, err, tt.want, tt.wantErr)
		}
	}
}

func healthServer(code int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
}

func TestCheckHealth(t *testing.T) {
	const (
		ok       = `{"status":"ok"}`
		degraded = `{"status":"degraded"}`
		bad      = `{"status":"unhealthy"}`
	)
	tests := []struct {
		name     string
		code     int
		body     string
		strict   bool
		wantCode int
	}{
		{"ok", 200, ok, false, 0},
		{"ok strict", 200, ok, true, 0},
		{"degraded", 200, degraded, false, 0},
		{"degraded strict", 200, degraded, true, 1},
		{"unhealthy", 503, bad, false, 1},
		{"unparseable strict", 200, "garbage", true, 1},
		{"unparseable lenient", 200, "garbage", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := healthServer(tt.code, tt.body)
			defer srv.Close()

			var stdout, stderr bytes.Buffer
			got := checkHealth(context.Background(), srv.URL, tt.strict, &stdout, &stderr)
			if got != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", got, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.body) {
				t.Errorf("stdout %q should contain the body %q", stdout.String(), tt.body)
			}
		})
	}
}

func TestCheckHealthUnreachable(t *testing.T) {
	srv := healthServer(200, "{}")
	url := srv.URL
	srv.Close()

	var stdout, stderr bytes.Buffer
	if got := checkHealth(context.Background(), url, false, &stdout, &stderr); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
	if stderr.Len() == 0 {
		t.Error("stderr should explain the failure")
	}
}

func TestRunHealthParsesFlagsAndEnv(t *testing.T) {
	srv := healthServer(200, `{"status":"degraded"}`)
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTP_ADDR", ":"+port)

	var stdout, stderr bytes.Buffer
	if got := run([]string{"health"}, &stdout, &stderr); got != 0 {
		t.Errorf("health: exit = %d, stderr %q", got, stderr.String())
	}
	if got := run([]string{"health", "--strict"}, &stdout, &stderr); got != 1 {
		t.Errorf("health --strict on degraded: exit = %d, want 1", got)
	}
	if got := run([]string{"health", "--bogus"}, &stdout, &stderr); got != 2 {
		t.Errorf("health --bogus: exit = %d, want 2", got)
	}
	t.Setenv("HTTP_ADDR", "nonsense")
	if got := run([]string{"health"}, &stdout, &stderr); got != 1 {
		t.Errorf("bad HTTP_ADDR: exit = %d, want 1", got)
	}
}

func TestServeHTTPShutsDownGracefully(t *testing.T) {
	ln, err := listenHealth("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hi") })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveHTTP(ctx, ln, h, zap.NewNop())
	}()

	code := checkHealth(context.Background(), "http://"+ln.Addr().String()+"/", false, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("request exit = %d, want 0", code)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveHTTP did not return after cancel")
	}
}

func TestListenHealthFailsOnBadAddr(t *testing.T) {
	if _, err := listenHealth("bad:addr:here"); err == nil || !strings.Contains(err.Error(), "HTTP_ADDR") {
		t.Errorf("err = %v, want one naming HTTP_ADDR", err)
	}
}

func TestHealthQueueAdapter(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	q := healthQueue{db: db}

	if n, cerr := q.CountUnfinished(ctx); cerr != nil || n != 0 {
		t.Fatalf("empty count = %d, %v", n, cerr)
	}
	if o, oerr := q.OldestUnfinished(ctx); oerr != nil || o != nil {
		t.Fatalf("empty oldest = %v err=%v", o, oerr)
	}
	if d, derr := q.LastDigestDate(ctx); derr != nil || d != "" {
		t.Fatalf("no digest: %q, %v", d, derr)
	}

	recv := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	c, err := capture.New(capture.KindAudio, capture.StepDownload, recv)
	if err != nil {
		t.Fatal(err)
	}
	c.Source, c.SourceChatID, c.SourceMessageID = "telegram", -1001234567890, 1
	c.Files = []capture.File{{Kind: capture.FileAudio, State: capture.FilePending, FileID: "FAKE-FILE"}}
	c.Status, c.WaitingOn = capture.StatusWaiting, capture.WaitingOn("stt")
	if err = db.Insert(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if n, cerr := q.CountUnfinished(ctx); cerr != nil || n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	got, err := q.OldestUnfinished(ctx)
	if err != nil || got == nil || !got.ReceivedAt.Equal(recv) || got.WaitingOn != "stt" {
		t.Errorf("oldest = %+v %v", got, err)
	}

	if got.Misconfig {
		t.Error("Misconfig = true for a plain waiting capture")
	}
	c.Misconfig, c.LastError = true, "http 401"
	if err = db.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got, err = q.OldestUnfinished(ctx); err != nil || got == nil || !got.Misconfig {
		t.Errorf("oldest after misconfig = %+v %v, want Misconfig", got, err)
	}

	dr := digest.Run{LocalDate: "2026-10-30", Status: digest.StatusSent, SentAt: recv}
	if err := db.RecordDigestRun(ctx, dr); err != nil {
		t.Fatal(err)
	}
	if d, derr := q.LastDigestDate(ctx); derr != nil || d != "2026-10-30" {
		t.Errorf("digest date = %q, %v", d, derr)
	}
	// An empty or skipped day sent nothing: last_digest keeps the last sent one.
	for _, r := range []digest.Run{
		{LocalDate: "2026-10-31", Status: digest.StatusEmpty, SentAt: recv},
		{LocalDate: "2026-11-01", Status: digest.StatusSkipped, SentAt: recv},
	} {
		if err := db.RecordDigestRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if d, derr := q.LastDigestDate(ctx); derr != nil || d != "2026-10-30" {
		t.Errorf("digest date after empty and skipped days = %q, %v, want 2026-10-30", d, derr)
	}
}

type fakeLane struct{ alive, storeOK bool }

func (f fakeLane) Alive() bool   { return f.alive }
func (f fakeLane) StoreOK() bool { return f.storeOK }

func TestBothLanesRequireEveryLane(t *testing.T) {
	up := fakeLane{alive: true, storeOK: true}
	tests := []struct {
		name              string
		lanes             bothLanes
		wantAlive, wantOK bool
	}{
		{"both up", bothLanes{up, up}, true, true},
		{"main down", bothLanes{fakeLane{}, up}, false, false},
		{"documents down", bothLanes{up, fakeLane{}}, false, false},
		{"documents store down", bothLanes{up, fakeLane{alive: true}}, true, false},
		{"main store down", bothLanes{fakeLane{alive: true}, up}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lanes.Alive(); got != tc.wantAlive {
				t.Errorf("Alive() = %v, want %v", got, tc.wantAlive)
			}
			if got := tc.lanes.StoreOK(); got != tc.wantOK {
				t.Errorf("StoreOK() = %v, want %v", got, tc.wantOK)
			}
		})
	}
}

func TestHealthNotOKWhenEitherLaneIsDown(t *testing.T) {
	up := fakeLane{alive: true, storeOK: true}
	for name, lanes := range map[string]bothLanes{"main": {fakeLane{}, up}, "documents": {up, fakeLane{}}} {
		rep := health.Compute(health.Input{
			Now: time.Now(), Started: time.Now(), Threshold: time.Hour,
			WorkerAlive: lanes.Alive(), StoreOK: lanes.StoreOK(),
		})
		if rep.Status == health.StatusOK {
			t.Errorf("%s lane down: status = %v, want not ok", name, rep.Status)
		}
	}
}
