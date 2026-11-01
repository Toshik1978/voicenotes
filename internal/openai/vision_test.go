package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

func newTestVision(t *testing.T, h http.HandlerFunc, mutate func(*VisionConfig)) *Vision {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := VisionConfig{BaseURL: srv.URL + "/v1/", APIKey: "FAKE-KEY", Model: "vision-model", Version: "1.2.3"}
	if mutate != nil {
		mutate(&cfg)
	}

	return NewVision(cfg, srv.Client())
}

func TestVisionRequest(t *testing.T) {
	image := []byte{0xff, 0xd8, 0x01, 0x02}
	var got struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	var gotPath, gotAuth, gotUA string
	v := newTestVision(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotUA = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"  a cat on a sofa \n"}}]}`)
	}, nil)

	text, out := v.Describe(context.Background(), image, "image/jpeg")
	if out.Class != capture.ClassOK || text != "a cat on a sofa" {
		t.Fatalf("got %q, %+v", text, out)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer FAKE-KEY" || gotUA != "VoiceNotes/1.2.3" {
		t.Errorf("request = %q %q %q", gotPath, gotAuth, gotUA)
	}
	if got.Model != "vision-model" || len(got.Messages) != 2 || got.Messages[0].Role != "system" ||
		got.Messages[1].Role != "user" {
		t.Fatalf("body = %+v", got)
	}
	var sys string
	if err := json.Unmarshal(got.Messages[0].Content, &sys); err != nil || sys == "" {
		t.Errorf("system content = %s (%v)", got.Messages[0].Content, err)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(got.Messages[1].Content, &parts); err != nil || len(parts) != 2 {
		t.Fatalf("user content = %s (%v)", got.Messages[1].Content, err)
	}
	if parts[0].Type != "text" || parts[0].Text == "" || parts[1].Type != "image_url" {
		t.Errorf("parts = %+v", parts)
	}
	if want := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(image); parts[1].ImageURL.URL != want {
		t.Errorf("data url = %q, want %q", parts[1].ImageURL.URL, want)
	}
}

func TestVisionNoKeyNoAuthHeader(t *testing.T) {
	var gotAuth string
	v := newTestVision(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
	}, func(c *VisionConfig) { c.APIKey = "" })
	if _, out := v.Describe(context.Background(), []byte("i"), "image/png"); out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v", out)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none", gotAuth)
	}
}

func TestVisionCapsRunes(t *testing.T) {
	long := strings.Repeat("я", 2500)
	v := newTestVision(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).
			Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": long}}}})
	}, nil)
	text, out := v.Describe(context.Background(), []byte("i"), "image/png")
	if out.Class != capture.ClassOK || len([]rune(text)) != 2000 {
		t.Errorf("runes = %d, outcome = %+v", len([]rune(text)), out)
	}
}

func TestVisionClassification(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   capture.Class
	}{
		{"server error", 503, "", capture.ClassTemporary},
		{"rate limit", 429, "", capture.ClassTemporary},
		{"unauthorized", 401, "", capture.ClassMisconfig},
		{"bad request", 400, "", capture.ClassPermanent},
		{"not json", 200, "oops", capture.ClassPermanent},
		{"no choices", 200, `{"choices":[]}`, capture.ClassPermanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newTestVision(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}, nil)
			text, out := v.Describe(context.Background(), []byte("i"), "image/png")
			if out.Class != tt.want || text != "" {
				t.Errorf("got %q, %+v, want class %v", text, out, tt.want)
			}
		})
	}
}

func TestVisionNetworkErrorAndBadURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	v := NewVision(VisionConfig{BaseURL: srv.URL}, nil)
	if _, out := v.Describe(context.Background(), []byte("i"), "image/png"); out.Class != capture.ClassTemporary {
		t.Errorf("network outcome = %+v", out)
	}
	v = NewVision(VisionConfig{BaseURL: "http://bad host/"}, nil)
	if _, out := v.Describe(context.Background(), []byte("i"), "image/png"); out.Class != capture.ClassMisconfig {
		t.Errorf("bad url outcome = %+v", out)
	}
}
