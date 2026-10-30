package digest

import (
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func note(uid string, age int, content string, task bool) NoteSummary {
	return NoteSummary{
		Name:        "memos/" + uid,
		URL:         "https://memos.example.org/memos/" + uid,
		Content:     content,
		CreateTime:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(age) * time.Hour),
		HasTaskList: task,
	}
}

func TestComposeEmpty(t *testing.T) {
	if text, ok := Compose(nil, nil, nil, 30); ok || text != "" {
		t.Fatalf("got %q, %v; want empty, false", text, ok)
	}
}

func TestComposeLayout(t *testing.T) {
	notes := []NoteSummary{
		note("b", 2, "Second new #inbox", false),
		note("a", 1, "First new\nsecond line", true),
		note("o", 0, "Old one", false),
	}
	seen := map[string]bool{"memos/o": true}
	got, ok := Compose(notes, seen, map[string]string{"memos/b": "LLM <b>"}, 30)
	if !ok {
		t.Fatal("ok = false")
	}
	want := "☀️ VoiceNotes — 2 new, 1 from earlier\n\n" +
		"• ☑ First new — <a href=\"https://memos.example.org/memos/a\">open</a>\n" +
		"• LLM &lt;b&gt; — <a href=\"https://memos.example.org/memos/b\">open</a>\n" +
		"\nFrom earlier (1)\n" +
		"• Old one — <a href=\"https://memos.example.org/memos/o\">open</a>"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestComposeOnlyEarlierOmitsNothingExtra(t *testing.T) {
	got, ok := Compose([]NoteSummary{note("o", 0, "x", false)}, map[string]bool{"memos/o": true}, nil, 30)
	if !ok || !strings.HasPrefix(got, "☀️ VoiceNotes — 0 new, 1 from earlier\n\nFrom earlier (1)\n") {
		t.Fatalf("got %q, %v", got, ok)
	}
}

func TestComposeCap(t *testing.T) {
	notes := make([]NoteSummary, 0, 6)
	seen := map[string]bool{}
	for i, uid := range []string{"n1", "n2", "o1", "o2", "o3", "o4"} {
		notes = append(notes, note(uid, i, uid, false))
		if uid[0] == 'o' {
			seen["memos/"+uid] = true
		}
	}
	got, _ := Compose(notes, seen, nil, 3)
	if !strings.Contains(got, "2 new, 1 from earlier") || !strings.HasSuffix(got, "…and 3 more") {
		t.Fatalf("got:\n%s", got)
	}
	got, _ = Compose(notes, seen, nil, 1)
	if !strings.Contains(got, "1 new, 0 from earlier") || strings.Contains(got, "From earlier") ||
		!strings.HasSuffix(got, "…and 5 more") {
		t.Fatalf("got:\n%s", got)
	}
	got, _ = Compose(notes, seen, nil, 0)
	if strings.Contains(got, "…and") || !strings.Contains(got, "From earlier (4)") {
		t.Fatalf("max<=0 should not cap:\n%s", got)
	}
}

func TestSummaryFor(t *testing.T) {
	long := strings.Repeat("я", 100)
	tests := []struct {
		name, content, llm, want string
	}{
		{"first non-empty line", "\n\n  Hello world  \nnext", "", "Hello world"},
		{"tags stripped", "#inbox #a/b Buy milk #later", "", "Buy milk"},
		{"tag-only line skipped", "#inbox\nReal text", "", "Real text"},
		{"hash in word kept", "C# and issue#5", "", "C# and issue#5"},
		{"empty", "#inbox", "", ""},
		{"long fallback", long, "", strings.Repeat("я", 79) + "…"},
		{"llm preferred", "x", "  Short  ", "Short"},
		{"llm truncated", "x", long, strings.Repeat("я", 79) + "…"},
		{"exactly 80", strings.Repeat("a", 80), "", strings.Repeat("a", 80)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryFor(tc.content, tc.llm); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func u16(s string) int { return len(utf16.Encode([]rune(s))) }

func TestSplitMessage(t *testing.T) {
	if got := SplitMessage("a\nb", 10); len(got) != 1 || got[0] != "a\nb" {
		t.Fatalf("got %q", got)
	}
	got := SplitMessage("aaaa\nbbbb\ncccc", 9)
	if len(got) != 2 || got[0] != "aaaa\nbbbb" || got[1] != "cccc" {
		t.Fatalf("got %q", got)
	}
	// emoji are 2 UTF-16 units each
	got = SplitMessage("😀😀\n😀😀", 5)
	if len(got) != 2 {
		t.Fatalf("got %q", got)
	}
	// a single oversize line is cut by rune
	got = SplitMessage(strings.Repeat("😀", 5), 4)
	if len(got) != 3 {
		t.Fatalf("got %q", got)
	}
	for _, p := range got {
		if u16(p) > 4 {
			t.Fatalf("part too long: %q", p)
		}
	}
	if got := SplitMessage("", 10); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestSplitMessageTinyLimitsTerminate(t *testing.T) {
	for _, limit := range []int{0, 1, 2} {
		parts := SplitMessage("a😀b\nc", limit)
		if got := strings.Join(parts, ""); got != "a😀bc" && got != "a😀b\nc" {
			t.Errorf("limit %d: parts %q lose text", limit, parts)
		}
	}
}

func TestSplitMessageInvalidUTF8(t *testing.T) {
	text := "ab\xffcd\xfeef"
	parts := SplitMessage(text, 3)
	if got := strings.Join(parts, ""); got != text {
		t.Errorf("parts %q rejoin to %q, want %q", parts, got, text)
	}
}
