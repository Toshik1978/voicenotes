package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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

// verbose_json segments carry Whisper's no_speech_prob. The values are kept
// only when every segment reports one; the shape is a real speaches answer
// for five seconds of silence.
func TestTranscribeNoSpeechProbs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []float64
	}{
		{
			"silence hallucination",
			`{"text":"Teksting av Nicolai Winther","language":"nn","duration":5.0,"segments":[` +
				`{"id":0,"text":" Teksting av Nicolai Winther","avg_logprob":-0.302,` +
				`"compression_ratio":0.78,"no_speech_prob":0.837}]}`,
			[]float64{0.837},
		},
		{
			"two segments",
			`{"text":"a b","segments":[{"text":" a","no_speech_prob":0.064},{"text":" b","no_speech_prob":0.9}]}`,
			[]float64{0.064, 0.9},
		},
		{
			"a segment without the field",
			`{"text":"a b","segments":[{"text":" a","no_speech_prob":0.9},{"text":" b"}]}`,
			nil,
		},
		{"no segments (json format)", `{"text":"hello"}`, nil},
		{"empty segment list", `{"text":"","segments":[]}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTestTranscriber(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			}, nil)
			res, out := tr.Transcribe(context.Background(), Request{ID: "1", Audio: []byte("A")})
			if out.Class != capture.ClassOK {
				t.Fatalf("outcome = %+v", out)
			}
			if !slices.Equal(res.NoSpeechProbs, tt.want) || (tt.want == nil) != (res.NoSpeechProbs == nil) {
				t.Errorf("NoSpeechProbs = %v, want %v", res.NoSpeechProbs, tt.want)
			}
		})
	}
}

// HasSpeech asks speaches' voice activity detector for speech regions. Only
// the file is sent: the endpoint's own "model" field names the VAD model, so
// the transcription model must not leak into it.
func TestHasSpeech(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"no regions", `[]`, false},
		{"one region", `[{"start":352,"end":736}]`, true},
		{"two regions", `[{"start":2656,"end":4544},{"start":6752,"end":8000}]`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAuth, gotName string
			var gotFields map[string][]string
			tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("parse multipart: %v", err)

					return
				}
				gotFields = r.MultipartForm.Value
				gotName = r.MultipartForm.File["file"][0].Filename
				_, _ = io.WriteString(w, tt.body)
			}, nil)
			got, out := tr.HasSpeech(
				context.Background(),
				Request{ID: "7", Ext: ".wav", Audio: []byte("RIFF"), Language: "ru"},
			)
			if out.Class != capture.ClassOK {
				t.Fatalf("outcome = %+v", out)
			}
			if got != tt.want {
				t.Errorf("HasSpeech = %v, want %v", got, tt.want)
			}
			if gotPath != "/v1/audio/speech/timestamps" || gotAuth != "Bearer FAKE-KEY" || gotName != "capture-7.wav" {
				t.Errorf("request = %q %q %q", gotPath, gotAuth, gotName)
			}
			if len(gotFields) != 0 {
				t.Errorf("fields = %v, want only the file", gotFields)
			}
		})
	}
}

func TestHasSpeechFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   capture.Class
	}{
		{"not found (not speaches)", http.StatusNotFound, `{"detail":"Not Found"}`, capture.ClassMisconfig},
		{"server error", http.StatusInternalServerError, ``, capture.ClassTemporary},
		{"malformed", http.StatusOK, `{"text":"x"}`, capture.ClassTemporary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTestTranscriber(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}, nil)
			_, out := tr.HasSpeech(context.Background(), Request{ID: "7", Audio: []byte("A")})
			if out.Class != tt.want {
				t.Errorf("class = %v, want %v (%+v)", out.Class, tt.want, out)
			}
		})
	}
}
