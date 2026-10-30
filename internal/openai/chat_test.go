package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

func newTestChat(t *testing.T, h http.HandlerFunc, mutate func(*ChatConfig)) *Chat {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := ChatConfig{BaseURL: srv.URL + "/v1/", APIKey: "FAKE-KEY", Model: "voicenotes", Version: "1.2.3"}
	if mutate != nil {
		mutate(&cfg)
	}

	return NewChat(cfg, srv.Client())
}

func testMessages() []Message {
	return []Message{
		{Role: "system", Content: "answer with JSON"},
		{Role: "user", Content: "hello"},
	}
}

func TestChatRequest(t *testing.T) {
	var gotBody map[string]json.RawMessage
	var gotPath, gotMethod, gotAuth, gotUA, gotCT string
	c := newTestChat(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth, gotUA, gotCT = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"text\":\"hi\"}"}}]}`)
	}, nil)

	got, out := c.Complete(context.Background(), testMessages())
	if out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v", out)
	}
	if got != `{"text":"hi"}` {
		t.Errorf("content = %q", got)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/chat/completions" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer FAKE-KEY" || gotUA != "VoiceNotes/1.2.3" || gotCT != "application/json" {
		t.Errorf("headers = %q %q %q", gotAuth, gotUA, gotCT)
	}
	if len(gotBody) != 2 {
		t.Errorf("body keys = %v, want exactly model, messages", gotBody)
	}
	if string(gotBody["model"]) != `"voicenotes"` {
		t.Errorf("model = %s", gotBody["model"])
	}
	var msgs []Message
	want := testMessages()
	if err := json.Unmarshal(gotBody["messages"], &msgs); err != nil || len(msgs) != 2 ||
		msgs[0] != want[0] || msgs[1] != want[1] {
		t.Errorf("messages = %s (%v)", gotBody["messages"], err)
	}
}

// Some models accept only their default temperature, so it is sent only when
// configured, and then even when it is 0.
func TestChatTemperature(t *testing.T) {
	for _, temp := range []float64{0, 1, 0.2} {
		var gotBody map[string]json.RawMessage
		c := newTestChat(t, func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}, func(cfg *ChatConfig) { cfg.Temperature = &temp })

		if _, out := c.Complete(context.Background(), testMessages()); out.Class != capture.ClassOK {
			t.Fatalf("outcome = %+v", out)
		}
		var got float64
		if err := json.Unmarshal(gotBody["temperature"], &got); err != nil || got != temp || len(gotBody) != 3 {
			t.Errorf("temperature %v: body = %v (%v)", temp, gotBody, err)
		}
	}
}

func TestChatWithoutKey(t *testing.T) {
	var authSet bool
	c := newTestChat(t, func(w http.ResponseWriter, r *http.Request) {
		_, authSet = r.Header["Authorization"]
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}, func(c *ChatConfig) { c.APIKey = "" })
	if got, out := c.Complete(context.Background(), testMessages()); out.Class != capture.ClassOK || got != "ok" {
		t.Fatalf("got %q, %+v", got, out)
	}
	if authSet {
		t.Error("Authorization header sent without a key")
	}
}

func TestChatErrorClasses(t *testing.T) {
	tests := []struct {
		status int
		want   capture.Class
	}{
		{500, capture.ClassTemporary},
		{408, capture.ClassTemporary},
		{429, capture.ClassTemporary},
		{401, capture.ClassMisconfig},
		{403, capture.ClassMisconfig},
		{404, capture.ClassMisconfig},
		{400, capture.ClassPermanent},
		{413, capture.ClassPermanent},
		{422, capture.ClassPermanent},
	}
	for _, tt := range tests {
		c := newTestChat(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tt.status)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key FAKE-KEY"}}`)
		}, nil)
		got, out := c.Complete(context.Background(), testMessages())
		if out.Class != tt.want || got != "" {
			t.Errorf("status %d: class = %v, content %q, want %v", tt.status, out.Class, got, tt.want)
		}
		if !strings.Contains(out.Reason, "http") || strings.Contains(out.Reason, "FAKE-KEY") {
			t.Errorf("status %d: reason %q", tt.status, out.Reason)
		}
	}
}

func TestChatUnusableBodyIsPermanent(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `<html>gateway</html>`,
		"truncated":     `{"choices":[`,
		"empty choices": `{"choices":[]}`,
		"no choices":    `{}`,
	} {
		c := newTestChat(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}, nil)
		got, out := c.Complete(context.Background(), testMessages())
		if out.Class != capture.ClassPermanent || got != "" {
			t.Errorf("%s: class = %v, content %q", name, out.Class, got)
		}
		if !strings.Contains(out.Reason, "malformed") {
			t.Errorf("%s: reason = %q", name, out.Reason)
		}
	}
}

func TestChatNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := NewChat(ChatConfig{BaseURL: url, APIKey: "FAKE-KEY", Model: "m"}, &http.Client{Timeout: time.Second})
	_, out := c.Complete(context.Background(), testMessages())
	if out.Class != capture.ClassTemporary || strings.Contains(out.Reason, "FAKE-KEY") {
		t.Errorf("outcome = %+v", out)
	}
}

func TestChatBadBaseURL(t *testing.T) {
	c := NewChat(ChatConfig{BaseURL: "http://[::1", Model: "m"}, nil)
	_, out := c.Complete(context.Background(), testMessages())
	if out.Class != capture.ClassMisconfig || !strings.Contains(out.Reason, "LLM_BASE_URL") {
		t.Errorf("outcome = %+v", out)
	}
}

func TestChatNetworkErrorOmitsURLCredentials(t *testing.T) {
	c := NewChat(
		ChatConfig{BaseURL: "http://user:secret@127.0.0.1:1/v1", Model: "m"},
		&http.Client{Timeout: time.Second},
	)
	_, out := c.Complete(context.Background(), testMessages())
	if out.Class != capture.ClassTemporary {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Reason == "" || strings.Contains(out.Reason, "user") || strings.Contains(out.Reason, "secret") {
		t.Errorf("reason = %q", out.Reason)
	}
}
