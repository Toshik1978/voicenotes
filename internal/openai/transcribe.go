// Package openai holds thin clients for OpenAI-compatible HTTP APIs.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

const defaultTimeout = 120 * time.Second

// Config configures a Transcriber.
type Config struct {
	// BaseURL includes the version prefix, for example https://stt.example.org/v1.
	BaseURL string
	// APIKey is optional; the Authorization header is sent only when it is set.
	APIKey string
	// Model is sent as the "model" field.
	Model string
	// ResponseFormat is "verbose_json" or "json".
	ResponseFormat string
	// Version is used for "User-Agent: VoiceNotes/<version>".
	Version string
}

// Request is one transcription call.
type Request struct {
	// ID names the uploaded file: capture-<ID><Ext>.
	ID string
	// Ext is the audio file extension, for example ".mp3"; empty or
	// malformed means ".ogg" (a voice note).
	Ext string
	// Audio is sent as-is, without transcoding.
	Audio []byte
	// Language forces a language; empty lets the service detect it.
	Language string
}

// Result is a successful transcription. Language is exactly what the service
// returned (for example "russian" or "ru") and may be empty; normalising it
// belongs to internal/language.
type Result struct {
	Text     string
	Language string
}

// Transcriber calls POST {BaseURL}/audio/transcriptions.
type Transcriber struct {
	cfg    Config
	client *http.Client
}

// NewTranscriber builds a Transcriber. A nil client gets a default one with a
// 120s timeout; callers pass their own to apply STT_TIMEOUT or inject tests.
func NewTranscriber(cfg Config, client *http.Client) *Transcriber {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &Transcriber{cfg: cfg, client: client}
}

// Transcribe sends the audio and classifies the result per research R10 via
// capture.ClassifyHTTP and capture.NetworkError. Every non-OK outcome is tagged
// capture.WaitingSTT. The Result is meaningful only when Outcome.Class is
// capture.ClassOK. Outcome reasons never contain the key or response bodies.
func (t *Transcriber) Transcribe(ctx context.Context, r Request) (Result, capture.Outcome) {
	body, contentType, err := t.buildBody(r)
	if err != nil {
		return Result{}, capture.Permanent("build request: " + err.Error()).On(capture.WaitingSTT)
	}
	endpoint := strings.TrimRight(t.cfg.BaseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return Result{}, capture.Misconfig("invalid STT_BASE_URL").On(capture.WaitingSTT)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "VoiceNotes/"+t.cfg.Version)
	if t.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return Result{}, capture.NetworkError(err).On(capture.WaitingSTT)
	}
	defer func() { _ = resp.Body.Close() }()

	if out := capture.ClassifyHTTP(resp.StatusCode); out.Class != capture.ClassOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return Result{}, out.On(capture.WaitingSTT)
	}

	var parsed struct {
		Text     string `json:"text"`
		Language string `json:"language"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Result{}, capture.Temporary("malformed response").On(capture.WaitingSTT)
	}

	return Result{Text: parsed.Text, Language: parsed.Language}, capture.OK()
}

func (t *Transcriber) buildBody(r Request) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "capture-"+r.ID+fileExt(r.Ext))
	if err != nil {
		return nil, "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := fw.Write(r.Audio); err != nil {
		return nil, "", fmt.Errorf("write audio: %w", err)
	}
	fields := [][2]string{
		{"model", t.cfg.Model},
		{"response_format", t.cfg.ResponseFormat},
		{"temperature", "0"},
	}
	if r.Language != "" {
		fields = append(fields, [2]string{"language", r.Language})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return nil, "", fmt.Errorf("write field %s: %w", f[0], err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart: %w", err)
	}

	return &buf, mw.FormDataContentType(), nil
}

// fileExt returns ext when it looks like ".<1-5 lowercase letters or
// digits>", otherwise ".ogg": a voice note, and what a backend that picks the
// decoder by name expects by default.
func fileExt(ext string) string {
	const maxExt = 6 // the dot plus five characters
	if len(ext) < 2 || len(ext) > maxExt || ext[0] != '.' {
		return ".ogg"
	}
	for _, ch := range ext[1:] {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return ".ogg"
		}
	}

	return ext
}
