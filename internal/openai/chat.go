package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// ChatConfig configures a Chat client.
type ChatConfig struct {
	// BaseURL includes the version prefix, for example https://llm.example.org/v1.
	BaseURL string
	// APIKey is optional; the Authorization header is sent only when it is set.
	APIKey string
	// Model is sent as the "model" field.
	Model string
	// Version is used for "User-Agent: VoiceNotes/<version>".
	Version string
}

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Chat calls POST {BaseURL}/chat/completions.
type Chat struct {
	cfg    ChatConfig
	client *http.Client
}

// NewChat builds a Chat client. A nil client gets a default one with a 60s
// timeout; callers pass their own to apply LLM_TIMEOUT or inject tests.
func NewChat(cfg ChatConfig, client *http.Client) *Chat {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Chat{cfg: cfg, client: client}
}

// Complete sends the messages with temperature 0 and returns
// choices[0].message.content. Only model, temperature and messages are sent, so
// any OpenAI-compatible gateway accepts the call. Errors are classified like
// Transcribe (research R10). A 2xx whose body is not JSON or has no choices is
// Permanent: retrying the same prompt will not fix it, and the enrich policy
// then skips the LLM. Outcomes carry no WaitingOn tag because the LLM never
// holds a capture back. Prompts, responses, the key and URL credentials never
// appear in an Outcome reason.
func (c *Chat) Complete(ctx context.Context, messages []Message) (string, capture.Outcome) {
	payload, err := json.Marshal(struct {
		Model       string    `json:"model"`
		Temperature int       `json:"temperature"`
		Messages    []Message `json:"messages"`
	}{c.cfg.Model, 0, messages})
	if err != nil {
		return "", capture.Permanent("build request")
	}
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", capture.Misconfig("invalid LLM_BASE_URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "VoiceNotes/"+c.cfg.Version)
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", capture.NetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if out := capture.ClassifyHTTP(resp.StatusCode); out.Class != capture.ClassOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return "", out
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || len(parsed.Choices) == 0 {
		return "", capture.Permanent("malformed response")
	}

	return parsed.Choices[0].Message.Content, capture.OK()
}
