package digest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/enrich"
)

type fakeChat struct {
	body    string
	outcome capture.Outcome
	calls   int
	msgs    []enrich.Message
	hasDL   bool
}

func (f *fakeChat) Complete(ctx context.Context, msgs []enrich.Message) (string, capture.Outcome) {
	f.calls++
	f.msgs = msgs
	_, f.hasDL = ctx.Deadline()

	return f.body, f.outcome
}

func TestLLMSummarizer(t *testing.T) {
	notes := []NoteSummary{
		{Name: "memos/a", Content: "first"},
		{Name: "memos/b", Content: "second"},
		{Name: "memos/c", Content: "third"},
	}
	tests := []struct {
		name string
		chat *fakeChat
		want map[string]string
	}{
		{
			"maps numbers to names", &fakeChat{body: `{"summaries":{"1":"one","3":"three"}}`, outcome: capture.OK()},
			map[string]string{"memos/a": "one", "memos/c": "three"},
		},
		{
			"invalid entry dropped", &fakeChat{body: `{"summaries":{"1":"one","9":"x","2":""}}`, outcome: capture.OK()},
			map[string]string{"memos/a": "one"},
		},
		{"bad body", &fakeChat{body: `nonsense`, outcome: capture.OK()}, nil},
		{
			"failed outcome",
			&fakeChat{body: `{"summaries":{"1":"one"}}`, outcome: capture.Outcome{Class: capture.ClassTemporary}},
			nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewLLMSummarizer(tc.chat, time.Minute)
			got := s.Summarize(context.Background(), notes)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
			if tc.chat.calls != 1 || !tc.chat.hasDL {
				t.Fatalf("calls=%d deadline=%v", tc.chat.calls, tc.chat.hasDL)
			}
			if !strings.Contains(tc.chat.msgs[1].Content, "third") {
				t.Fatal("request lacks note text")
			}
		})
	}
}
