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
	// Temperature is sent only when set; nil leaves the model's default.
	Temperature *float64
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

// Complete sends the messages and returns choices[0].message.content. Only
// model, messages and, when configured, temperature are sent, so any
// OpenAI-compatible gateway accepts the call; some models reject any
// temperature but their default. Errors are classified like
// Transcribe (research R10). A 2xx whose body is not JSON or has no choices is
// Permanent: retrying the same prompt will not fix it, and the enrich policy
// then skips the LLM. Outcomes carry no WaitingOn tag because the LLM never
// holds a capture back. Prompts, responses, the key and URL credentials never
// appear in an Outcome reason.
func (c *Chat) Complete(ctx context.Context, messages []Message) (string, capture.Outcome) {
	payload, err := json.Marshal(struct {
		Model       string    `json:"model"`
		Temperature *float64  `json:"temperature,omitempty"`
		Messages    []Message `json:"messages"`
	}{c.cfg.Model, c.cfg.Temperature, messages})
	if err != nil {
		return "", capture.Permanent("build request")
	}

	return postChatCompletion(
		ctx,
		c.client,
		endpointSpec{c.cfg.BaseURL, c.cfg.APIKey, c.cfg.Version, "LLM_BASE_URL"},
		payload,
	)
}

// endpointSpec identifies an OpenAI-compatible chat endpoint; setting names
// the base URL variable for misconfiguration outcomes.
type endpointSpec struct {
	baseURL, apiKey, version, setting string
}

// postChatCompletion posts payload to {baseURL}/chat/completions and returns
// choices[0].message.content, classifying failures (shared by Chat and Vision).
func postChatCompletion(
	ctx context.Context,
	client *http.Client,
	ep endpointSpec,
	payload []byte,
) (string, capture.Outcome) {
	endpoint := strings.TrimRight(ep.baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", capture.Misconfig("invalid " + ep.setting)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "VoiceNotes/"+ep.version)
	if ep.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+ep.apiKey)
	}

	resp, err := client.Do(req)
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
