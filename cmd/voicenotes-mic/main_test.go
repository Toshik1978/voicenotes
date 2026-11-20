package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/config"
)

const fakeToken = "FAKE-0123456789abcdef0123456789abcdef"

func TestRunVersion(t *testing.T) {
	oldV, oldC, oldB := Version, Commit, Buildstamp
	t.Cleanup(func() { Version, Commit, Buildstamp = oldV, oldC, oldB })
	Version, Commit, Buildstamp = "1.2.3", "abc1234", "2026-01-02T03:04:05Z"

	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"voicenotes-mic", "1.2.3", "abc1234", "2026-01-02T03:04:05Z"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q missing %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestDispatchStartsRecorderByDefault(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}} {
		var stdout, stderr bytes.Buffer
		started := false
		code := dispatch(args, &stdout, &stderr, func() int {
			started = true
			return 7
		})

		if !started {
			t.Errorf("args %v: recorder not started", args)
		}
		if code != 7 {
			t.Errorf("args %v: exit code = %d, want the recorder's 7", args, code)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Errorf("args %v: unexpected output %q %q", args, stdout.String(), stderr.String())
		}
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{"bogus"}, &stdout, &stderr, func() int {
		t.Error("the recorder must not start for an unknown command")
		return 0
	})

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage: voicenotes-mic [run|health|version]") {
		t.Errorf("stderr %q should contain usage", stderr.String())
	}
}

func TestHealthURL(t *testing.T) {
	tests := []struct {
		addr    string
		want    string
		wantErr bool
	}{
		{"", "http://127.0.0.1:8081/healthz", false},
		{"127.0.0.1:8081", "http://127.0.0.1:8081/healthz", false},
		{":9000", "http://127.0.0.1:9000/healthz", false},
		{"0.0.0.0:8081", "http://127.0.0.1:8081/healthz", false},
		{"[::]:8081", "http://127.0.0.1:8081/healthz", false},
		{"192.0.2.10:8081", "http://192.0.2.10:8081/healthz", false},
		{"[::1]:8081", "http://[::1]:8081/healthz", false},
		{"bogus", "", true},
		{"127.0.0.1:", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got, err := healthURL(tt.addr)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "MIC_HTTP_ADDR") {
					t.Fatalf("healthURL(%q) error = %v, want one naming MIC_HTTP_ADDR", tt.addr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("healthURL(%q): %v", tt.addr, err)
			}
			if got != tt.want {
				t.Errorf("healthURL(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func healthServer(t *testing.T, code int, status string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"status":%q}`, status)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestCheckHealth(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		status string
		strict bool
		want   int
	}{
		{"ok", http.StatusOK, "ok", false, 0},
		{"degraded", http.StatusOK, "degraded", false, 0},
		{"degraded strict", http.StatusOK, "degraded", true, 1},
		{"ok strict", http.StatusOK, "ok", true, 0},
		{"unhealthy", http.StatusServiceUnavailable, "unhealthy", false, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := healthServer(t, tt.code, tt.status)
			var stdout, stderr bytes.Buffer
			got := checkHealth(t.Context(), srv.URL+"/healthz", tt.strict, &stdout, &stderr)

			if got != tt.want {
				t.Errorf("exit code = %d, want %d (stderr %q)", got, tt.want, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.status) {
				t.Errorf("stdout %q should echo the body", stdout.String())
			}
		})
	}
}

func TestCheckHealthUnreachable(t *testing.T) {
	ln := listenLocal(t)
	addr := ln.Addr().String()
	_ = ln.Close()

	var stdout, stderr bytes.Buffer
	if got := checkHealth(t.Context(), "http://"+addr+"/healthz", false, &stdout, &stderr); got != 1 {
		t.Errorf("exit code = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "health:") {
		t.Errorf("stderr %q should explain the failure", stderr.String())
	}
}

func TestRunHealthSubcommand(t *testing.T) {
	srv := healthServer(t, http.StatusOK, "degraded")
	t.Setenv("MIC_HTTP_ADDR", strings.TrimPrefix(srv.URL, "http://"))

	var stdout, stderr bytes.Buffer
	if got := run([]string{"health"}, &stdout, &stderr); got != 0 {
		t.Errorf("health = %d, want 0 (stderr %q)", got, stderr.String())
	}
	if got := run([]string{"health", "--strict"}, &stdout, &stderr); got != 1 {
		t.Errorf("health --strict = %d, want 1 on degraded", got)
	}
	if got := run([]string{"health", "--bogus"}, &stdout, &stderr); got != 2 {
		t.Errorf("health --bogus = %d, want 2", got)
	}

	t.Setenv("MIC_HTTP_ADDR", "bogus")
	stderr.Reset()
	if got := run([]string{"health"}, &stdout, &stderr); got != 1 {
		t.Errorf("health with a bad MIC_HTTP_ADDR = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "MIC_HTTP_ADDR") {
		t.Errorf("stderr %q should name MIC_HTTP_ADDR", stderr.String())
	}
}

func TestRunRecorderConfigError(t *testing.T) {
	var stderr bytes.Buffer
	code := runRecorder(t.Context(), []string{"MIC_UPLOAD_TOKEN=FAKE-short"}, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	for _, want := range []string{"MIC_SERVER_URL", "MIC_UPLOAD_TOKEN"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q should name %s", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "FAKE-short") {
		t.Errorf("stderr %q leaks the token value", stderr.String())
	}
}

// checkInServer is a fake service that counts check-ins and accepts every
// /mic/v1 request.
func checkInServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var checkIns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/mic/v1/recorders/") {
			checkIns.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	return srv, &checkIns
}

// recorderEnv is a valid recorder environment with a fixed (absent) device
// pair, so no hardware and no /sys walk is involved.
func recorderEnv(t *testing.T, serverURL, httpAddr string) []string {
	t.Helper()
	dir := t.TempDir()

	return []string{
		"MIC_SERVER_URL=" + serverURL,
		"MIC_UPLOAD_TOKEN=" + fakeToken,
		"MIC_RECORDER_NAME=bedside",
		"MIC_CAPTURE_DEVICE=hw:9",
		"MIC_INPUT_DEVICE=" + filepath.Join(dir, "no-such-event"),
		"MIC_SPOOL_DIR=" + filepath.Join(dir, "spool"),
		"MIC_SPOOL_MIN_FREE=40000000",
		"MIC_HTTP_ADDR=" + httpAddr,
		"LOG_LEVEL=error",
	}
}

func TestRunRecorderBusyHTTPAddr(t *testing.T) {
	ln := listenLocal(t)
	t.Cleanup(func() { _ = ln.Close() })
	srv, _ := checkInServer(t)

	var stderr bytes.Buffer
	if code := runRecorder(t.Context(), recorderEnv(t, srv.URL, ln.Addr().String()), &stderr); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "MIC_HTTP_ADDR") {
		t.Errorf("stderr %q should name MIC_HTTP_ADDR", stderr.String())
	}
}

func TestRunRecorderStartsAndStops(t *testing.T) {
	srv, checkIns := checkInServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)

	var stderr bytes.Buffer
	go func() { done <- runRecorder(ctx, recorderEnv(t, srv.URL, "127.0.0.1:0"), &stderr) }()

	deadline := time.After(10 * time.Second)
	for checkIns.Load() == 0 {
		select {
		case code := <-done:
			t.Fatalf("recorder exited early with %d: %s", code, stderr.String())
		case <-deadline:
			t.Fatal("no check-in within 10s")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recorder did not stop after cancellation")
	}
}

func TestServeHealthz(t *testing.T) {
	srv, _ := checkInServer(t)
	env := recorderEnv(t, srv.URL, "127.0.0.1:0")
	cfg, err := config.ParseRecorder(env)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := newRecorder(cfg, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	ln := listenLocal(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, ln, rec, cfg.PendingThreshold, zap.NewNop()) }()

	url := "http://" + ln.Addr().String() + "/healthz"
	var rep map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		rep = getJSON(t, url)
		if rep["status"] == "degraded" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The fixed input device does not exist, so the receiver never connects.
	if rep["status"] != "degraded" || rep["receiver"] != "missing" {
		t.Errorf("healthz = %v, want degraded with the receiver missing", rep)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("serve = %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after cancellation")
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}

	return m
}

// listenLocal binds a free loopback port.
func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	return ln
}
