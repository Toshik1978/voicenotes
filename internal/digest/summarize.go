package digest

import (
	"context"
	"strconv"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/enrich"
)

// Chat is the LLM chat call the summarizer needs; it matches pipeline.Enricher
// so one adapter serves both.
type Chat interface {
	Complete(ctx context.Context, msgs []enrich.Message) (string, capture.Outcome)
}

// LLMSummarizer implements Summarizer with one batched chat call per digest.
// Any failure yields no summaries, so every line falls back to the note's
// first line. It never retries and never logs note content.
type LLMSummarizer struct {
	chat    Chat
	timeout time.Duration
}

var _ Summarizer = (*LLMSummarizer)(nil)

// NewLLMSummarizer bounds each call by timeout (LLM_TIMEOUT).
func NewLLMSummarizer(chat Chat, timeout time.Duration) *LLMSummarizer {
	return &LLMSummarizer{chat: chat, timeout: timeout}
}

// Summarize returns note name -> summary for the notes the LLM summarized
// validly; the rest are absent.
func (s *LLMSummarizer) Summarize(ctx context.Context, notes []NoteSummary) map[string]string {
	if len(notes) == 0 {
		return nil
	}
	texts := make([]string, len(notes))
	for i, n := range notes {
		texts[i] = n.Content
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	body, out := s.chat.Complete(ctx, enrich.BuildSummaryRequest(texts))
	if out.Class != capture.ClassOK {
		return nil
	}
	parsed, err := enrich.ParseSummaries(body, len(notes))
	if err != nil {
		return nil
	}
	byName := make(map[string]string, len(parsed))
	for i, n := range notes {
		if v, ok := parsed[strconv.Itoa(i+1)]; ok {
			byName[n.Name] = v
		}
	}

	return byName
}
