package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

func newTestTranscriber(t *testing.T, h http.HandlerFunc, mutate func(*Config)) *Transcriber {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := Config{
		BaseURL:        srv.URL + "/v1/",
		APIKey:         "FAKE-KEY",
		Model:          "m1",
		ResponseFormat: "verbose_json",
		Version:        "1.2.3",
	}
	if mutate != nil {
		mutate(&cfg)
	}

	return NewTranscriber(cfg, srv.Client())
}

func TestTranscribeRequest(t *testing.T) {
	audio := []byte("OggS\x00binary\xff")
	var gotFields map[string]string
	var gotFile []byte
	var gotName string
	var gotPath, gotMethod, gotAuth, gotUA string
	tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth, gotUA = r.URL.Path, r.Method, r.Header.Get(
			"Authorization",
		), r.Header.Get(
			"User-Agent",
		)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		gotFields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			gotFields[k] = v[0]
		}
		fh := r.MultipartForm.File["file"][0]
		gotName = fh.Filename
		f, _ := fh.Open()
		gotFile, _ = io.ReadAll(f)
		_, _ = io.WriteString(w, `{"text":"hello","language":"russian"}`)
	}, nil)

	res, out := tr.Transcribe(context.Background(), Request{ID: "42", Audio: audio, Language: "ru"})
	if out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v", out)
	}
	if res.Text != "hello" || res.Language != "russian" {
		t.Errorf("result = %+v", res)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/audio/transcriptions" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer FAKE-KEY" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotUA != "VoiceNotes/1.2.3" {
		t.Errorf("ua = %q", gotUA)
	}
	if gotName != "capture-42.ogg" || !bytes.Equal(gotFile, audio) {
		t.Errorf("file = %q %q", gotName, gotFile)
	}
	want := map[string]string{"model": "m1", "response_format": "verbose_json", "language": "ru", "temperature": "0"}
	for k, v := range want {
		if gotFields[k] != v {
			t.Errorf("field %s = %q, want %q", k, gotFields[k], v)
		}
	}
	if len(gotFields) != len(want) {
		t.Errorf("fields = %v", gotFields)
	}
}

func TestTranscribeFileNameExtension(t *testing.T) {
	tests := []struct{ ext, want string }{
		{"", "capture-7.ogg"},
		{".mp3", "capture-7.mp3"},
		{".m4a", "capture-7.m4a"},
		{".wav", "capture-7.wav"},
		{"mp3", "capture-7.ogg"},
		{"./x", "capture-7.ogg"},
		{".toolongext", "capture-7.ogg"},
	}
	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			var got string
			tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) {
				if _, fh, err := r.FormFile("file"); err == nil {
					got = fh.Filename
				}
				_, _ = io.WriteString(w, `{"text":"x"}`)
			}, nil)
			_, out := tr.Transcribe(context.Background(), Request{ID: "7", Ext: tt.ext, Audio: []byte("a")})
			if out.Class != capture.ClassOK {
				t.Fatalf("outcome = %+v", out)
			}
			if got != tt.want {
				t.Errorf("filename = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTranscribeOptionalParts(t *testing.T) {
	var gotAuthSet bool
	var gotFields map[string][]string
	tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) {
		_, gotAuthSet = r.Header["Authorization"]
		_ = r.ParseMultipartForm(1 << 20)
		gotFields = r.MultipartForm.Value
		_, _ = io.WriteString(w, `{"text":"plain"}`)
	}, func(c *Config) { c.APIKey = ""; c.ResponseFormat = "json" })

	res, out := tr.Transcribe(context.Background(), Request{ID: "1", Audio: []byte("x")})
	if out.Class != capture.ClassOK {
		t.Fatalf("outcome = %+v", out)
	}
	if res.Text != "plain" || res.Language != "" {
		t.Errorf("result = %+v", res)
	}
	if gotAuthSet {
		t.Error("Authorization header sent without a key")
	}
	if _, ok := gotFields["language"]; ok {
		t.Error("language sent when not forced")
	}
	if gotFields["response_format"][0] != "json" {
		t.Errorf("response_format = %v", gotFields["response_format"])
	}
}

func TestTranscribeErrorClasses(t *testing.T) {
	tests := []struct {
		status int
		want   capture.Class
	}{
		{500, capture.ClassTemporary},
		{503, capture.ClassTemporary},
		{408, capture.ClassTemporary},
		{429, capture.ClassTemporary},
		{401, capture.ClassMisconfig},
		{403, capture.ClassMisconfig},
		{404, capture.ClassMisconfig},
		{400, capture.ClassPermanent},
		{413, capture.ClassPermanent},
		{415, capture.ClassPermanent},
		{422, capture.ClassPermanent},
	}
	for _, tt := range tests {
		tr := newTestTranscriber(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tt.status)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key FAKE-KEY"}}`)
		}, nil)
		_, out := tr.Transcribe(context.Background(), Request{ID: "1", Audio: []byte("x")})
		if out.Class != tt.want {
			t.Errorf("status %d: class = %v, want %v", tt.status, out.Class, tt.want)
		}
		if out.WaitingOn != capture.WaitingSTT {
			t.Errorf("status %d: waiting on %q", tt.status, out.WaitingOn)
		}
		if !strings.Contains(out.Reason, "http") || strings.Contains(out.Reason, "FAKE-KEY") {
			t.Errorf("status %d: reason %q", tt.status, out.Reason)
		}
	}
}

func TestTranscribeMalformedBody(t *testing.T) {
	tr := newTestTranscriber(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"text":`)
	}, nil)
	_, out := tr.Transcribe(context.Background(), Request{ID: "1", Audio: []byte("x")})
	if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingSTT {
		t.Errorf("outcome = %+v", out)
	}
	if !strings.Contains(out.Reason, "malformed") {
		t.Errorf("reason = %q", out.Reason)
	}
}

func TestTranscribeNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	tr := NewTranscriber(
		Config{BaseURL: url, APIKey: "FAKE-KEY", Model: "m", ResponseFormat: "json"},
		&http.Client{Timeout: time.Second},
	)
	_, out := tr.Transcribe(context.Background(), Request{ID: "1", Audio: []byte("x")})
	if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingSTT {
		t.Errorf("outcome = %+v", out)
	}
	if strings.Contains(out.Reason, "FAKE-KEY") {
		t.Errorf("reason leaks key: %q", out.Reason)
	}
}

func TestTranscribeBadBaseURL(t *testing.T) {
	tr := NewTranscriber(Config{BaseURL: "http://[::1", Model: "m"}, nil)
	_, out := tr.Transcribe(context.Background(), Request{ID: "1", Audio: []byte("x")})
	if out.Class != capture.ClassMisconfig || out.WaitingOn != capture.WaitingSTT {
		t.Errorf("outcome = %+v", out)
	}
}

func TestTranscribeNetworkErrorOmitsURLCredentials(t *testing.T) {
	tr := NewTranscriber(
		Config{BaseURL: "http://user:secret@127.0.0.1:1/v1", Model: "m", ResponseFormat: "json"},
		&http.Client{Timeout: time.Second},
	)
	_, out := tr.Transcribe(context.Background(), Request{ID: "1", Audio: []byte("x")})
	if out.Class != capture.ClassTemporary || out.WaitingOn != capture.WaitingSTT {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Reason == "" || strings.Contains(out.Reason, "user") || strings.Contains(out.Reason, "secret") {
		t.Errorf("reason = %q", out.Reason)
	}
}
