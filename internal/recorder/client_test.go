package recorder

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
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/mic"
)

const testToken = "FAKE-0123456789abcdef0123456789abcdef"

func testStart() time.Time {
	return time.Date(2026, 11, 20, 7, 30, 0, 123456789, time.FixedZone("x", 3600))
}

func newTestClient(t *testing.T, h http.HandlerFunc, prefix string) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL+prefix, testToken, "bedside", nil)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestUploadRequest(t *testing.T) {
	data := bytes.Repeat([]byte("abcd"), 1000)
	sum := sha256.Sum256(data)

	var called bool

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		got, _ := io.ReadAll(r.Body)

		if r.Method != http.MethodPut || r.URL.Path != "/pfx/mic/v1/recordings/42" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}

		want := map[string]string{
			"Authorization":          "Bearer " + testToken,
			"Content-Type":           "audio/wav",
			"X-Content-Sha256":       hex.EncodeToString(sum[:]),
			"X-Recording-Started-At": "2026-11-20T06:30:00.123456789Z",
			"X-Recording-End-Reason": "tap",
			"X-Recorder":             "bedside",
		}
		for k, v := range want {
			if r.Header.Get(k) != v {
				t.Errorf("header %s = %q, want %q", k, r.Header.Get(k), v)
			}
		}

		if r.ContentLength != int64(len(data)) || !bytes.Equal(got, data) {
			t.Errorf("body: length %d, equal %v", r.ContentLength, bytes.Equal(got, data))
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	}, "/pfx/")

	rec := Recording{ID: 42, StartedAt: testStart(), Recorder: "bedside", EndReason: mic.EndReason("tap")}

	res, err := c.Upload(t.Context(), rec, bytes.NewReader(data), int64(len(data)))
	if err != nil || res != Stored || !called {
		t.Fatalf("Upload = %v, %v (called %v)", res, err, called)
	}
}

func TestUploadResults(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
		want Result
	}{
		{"created", 201, `{"status":"stored"}`, Stored},
		{"duplicate", 200, `{"status":"duplicate"}`, Duplicate},
		{"corrupt", 400, `{"status":"corrupt"}`, Retry},
		{"other bad request", 400, `{"status":"invalid","reason":"bad header"}`, Rejected},
		{"bad request without body", 400, ``, Rejected},
		{"unavailable", 503, `{"status":"unavailable"}`, Retry},
		{"internal", 500, ``, Retry},
		{"unauthorized", 401, `{"status":"unauthorized"}`, Misconfigured},
		{"too large", 413, `{"status":"too_large"}`, Rejected},
		{"unsupported", 415, `{"status":"unsupported"}`, Rejected},
		{"unforeseen", 418, ``, Retry},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}, "")

			res, err := c.Upload(t.Context(), Recording{ID: 1, StartedAt: testStart()}, strings.NewReader("data"), 4)
			if res != tt.want {
				t.Errorf("result = %v, want %v", res, tt.want)
			}

			ok := tt.want == Stored || tt.want == Duplicate
			if ok != (err == nil) {
				t.Errorf("err = %v", err)
			}

			if err != nil && strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
}

func TestUploadNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	c, err := NewClient(url, testToken, "bedside", nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := c.Upload(t.Context(), Recording{ID: 1, StartedAt: testStart()}, strings.NewReader("data"), 4)
	if res != Retry || err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("Upload = %v, %v", res, err)
	}
}

func TestUploadBadBody(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("must not send") }, "")

	// Shorter than declared.
	if res, err := c.Upload(t.Context(), Recording{ID: 1}, strings.NewReader("ab"), 4); res != Retry || err == nil {
		t.Errorf("short body = %v, %v", res, err)
	}
}

func TestUploadContextCanceled(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {}, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if res, err := c.Upload(ctx, Recording{ID: 1}, strings.NewReader("data"), 4); res != Retry || err == nil {
		t.Errorf("Upload = %v, %v", res, err)
	}
}

func TestStarted(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mic/v1/recordings/7/started" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}

		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers = %v", r.Header)
		}

		var got map[string]string
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}

		if len(got) != 2 || got["started_at"] != "2026-11-20T06:30:00.123456789Z" || got["recorder"] != "bedside" {
			t.Errorf("body = %v", got)
		}

		w.WriteHeader(http.StatusAccepted)
	}, "")

	if err := c.Started(t.Context(), 7, testStart()); err != nil {
		t.Fatal(err)
	}
}

func TestDiscarded(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/mic/v1/recordings/7/started" ||
			r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("request = %s %s %v", r.Method, r.URL.Path, r.Header)
		}

		w.WriteHeader(http.StatusNoContent)
	}, "")

	if err := c.Discarded(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
}

func TestNoticeErrorsAreNotRetried(t *testing.T) {
	var calls int

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++

		w.WriteHeader(http.StatusInternalServerError)
	}, "")

	if err := c.Started(t.Context(), 7, testStart()); err == nil {
		t.Error("Started: want an error")
	}

	if err := c.Discarded(t.Context(), 7); err == nil {
		t.Error("Discarded: want an error")
	}

	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestCheckIn(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		want   string
	}{
		{
			"idle",
			Status{ReceiverPresent: true},
			`{"receiver_present":true,"storage_full":false,"pending":0,"oldest_pending_at":null}`,
		},
		{
			"pending",
			Status{Pending: 3, StorageFull: true, OldestPendingAt: testStart()},
			`{"receiver_present":false,"storage_full":true,"pending":3,"oldest_pending_at":"2026-11-20T06:30:00.123456789Z"}`,
		},
		{
			"audio cannot start: taps won't record",
			Status{ReceiverPresent: true, AudioError: true},
			`{"receiver_present":false,"storage_full":false,"pending":0,"oldest_pending_at":null}`,
		},
		{
			"a recording cannot be finished",
			Status{ReceiverPresent: true, FinishError: true},
			`{"receiver_present":false,"storage_full":false,"pending":0,"oldest_pending_at":null}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/mic/v1/recorders/bedside" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}

				b, _ := io.ReadAll(r.Body)

				var got, want map[string]any
				if json.Unmarshal(b, &got) != nil || json.Unmarshal([]byte(tt.want), &want) != nil {
					t.Fatalf("bad JSON: %s", b)
				}

				if len(got) != len(want) {
					t.Errorf("body = %s", b)
				}

				for k, v := range want {
					if got[k] != v {
						t.Errorf("%s = %v, want %v (%s)", k, got[k], v, b)
					}
				}

				w.WriteHeader(http.StatusNoContent)
			}, "")

			if err := c.Send(t.Context(), tt.status); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckInFailure(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"invalid","reason":"bad name"}`))
	}, "")

	err := c.Send(t.Context(), Status{})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad name") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckInUnauthorized(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}, "")

	if err := c.Send(t.Context(), Status{}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestNewClient(t *testing.T) {
	tests := []struct {
		name, url, token, rec string
		ok                    bool
	}{
		{"fine", "https://mic.example.org", testToken, "bedside", true},
		{"no scheme", "mic.example.org", testToken, "bedside", false},
		{"bad scheme", "ftp://mic.example.org", testToken, "bedside", false},
		{"no token", "https://mic.example.org", "", "bedside", false},
		{"no name", "https://mic.example.org", testToken, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.url, tt.token, tt.rec, nil)
			if (err == nil) != tt.ok {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestBaseURLJoin(t *testing.T) {
	for _, base := range []string{"https://h.example.org", "https://h.example.org/", "https://h.example.org/a/b/"} {
		c, err := NewClient(base, testToken, "bedside", nil)
		if err != nil {
			t.Fatal(err)
		}

		want := strings.TrimRight(base, "/") + "/mic/v1/recordings/5"
		if got := c.recordingURL(5); got != want {
			t.Errorf("recordingURL = %q, want %q", got, want)
		}
	}
}
