package render

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const big = 8192

func TestRender(t *testing.T) {
	base := Input{InboxTag: "inbox", TodoTag: "todo"}
	tests := []struct {
		name string
		mod  func(Input) Input
		want string
	}{
		{"transcript", func(i Input) Input { i.Text = "Buy milk."; return i }, "Buy milk.\n\n#inbox"},
		{"trims transcript", func(i Input) Input { i.Text = "  hi \n"; return i }, "hi\n\n#inbox"},
		{"empty transcript", func(i Input) Input { return i }, "(no speech recognized)\n\n#inbox"},
		{
			"failure",
			func(i Input) Input { i.Text = "ignored"; i.Failure = "speech-to-text rejected the audio"; return i },
			"(audio saved without text: speech-to-text rejected the audio)\n\n#inbox",
		},
		{"custom inbox tag", func(i Input) Input { i.Text = "x"; i.InboxTag = "capture"; return i }, "x\n\n#capture"},
		{"checklist and todo tag", func(i Input) Input {
			i.Text = "Plan"
			i.Actions = []string{"call dentist", "buy milk"}
			return i
		}, "Plan\n\n- [ ] call dentist\n- [ ] buy milk\n\n#inbox #todo"},
		{"original quote", func(i Input) Input {
			i.Text = "Hello"
			i.Original, i.OriginalLang = "Привет\nмир", "ru"
			return i
		}, "Hello\n\n*Original (ru):*\n> Привет\n> мир\n\n#inbox"},
		{"suggested tags deduped", func(i Input) Input {
			i.Text = "x"
			i.Actions = []string{"a"}
			i.SuggestedTags = []string{"garden", "inbox", "garden", "todo", "health"}
			return i
		}, "x\n\n- [ ] a\n\n#inbox #todo #garden #health"},
		{"full order", func(i Input) Input {
			i.Text = "Main"
			i.Actions = []string{"a"}
			i.Original, i.OriginalLang = "Orig", "de"
			i.SuggestedTags = []string{"t"}
			return i
		}, "Main\n\n- [ ] a\n\n*Original (de):*\n> Orig\n\n#inbox #todo #t"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Render(tt.mod(base), big); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderTruncation(t *testing.T) {
	long := strings.Repeat("слово ", 400) // 2-byte runes, multi-byte safety
	in := Input{
		InboxTag: "inbox", TodoTag: "todo", Text: long,
		Actions: []string{"keep me"}, SuggestedTags: []string{"keep"},
	}

	t.Run("main text truncated, tags and checklist kept", func(t *testing.T) {
		got := Render(in, 300)
		if len(got) > 300 {
			t.Fatalf("len %d > 300", len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatal("invalid UTF-8")
		}
		for _, s := range []string{"(truncated)", "- [ ] keep me", "#inbox #todo #keep"} {
			if !strings.Contains(got, s) {
				t.Errorf("missing %q in %q", s, got)
			}
		}
	})

	t.Run("quote shortened before main text", func(t *testing.T) {
		q := in
		q.Text = "short main"
		q.Original, q.OriginalLang = strings.Repeat("x", 2000), "ru"
		got := Render(q, 300)
		if len(got) > 300 {
			t.Fatalf("len %d > 300", len(got))
		}
		if !strings.Contains(got, "short main\n") || strings.Count(got, "(truncated)") != 1 {
			t.Errorf("unexpected %q", got)
		}
		if strings.Index(got, "(truncated)") < strings.Index(got, "*Original") {
			t.Errorf("truncation marker not in quote: %q", got)
		}
	})

	t.Run("quote dropped when no room, then main shortened", func(t *testing.T) {
		q := in
		q.Original, q.OriginalLang = "orig", "ru"
		got := Render(q, 120)
		if len(got) > 120 || !strings.Contains(got, "#inbox #todo #keep") {
			t.Errorf("bad %q", got)
		}
	})
}

func TestRenderQuoteDrop(t *testing.T) {
	t.Run("dropped quote does not truncate main text that fits", func(t *testing.T) {
		got := Render(Input{InboxTag: "inbox", Text: "short", Original: "orig", OriginalLang: "ru"}, 20)
		if want := "short\n\n#inbox"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("fully shortened quote keeps a truncation marker", func(t *testing.T) {
		in := Input{InboxTag: "inbox", Text: "short", Original: strings.Repeat("x", 500), OriginalLang: "ru"}
		want := "short\n\n*Original (ru):*\n> (truncated)\n\n#inbox"
		if got := Render(in, len(want)); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("output never starts with whitespace", func(t *testing.T) {
		got := Render(Input{InboxTag: "inbox", Text: strings.Repeat("a", 100)}, 20)
		if got == "" || got[0] == ' ' || !strings.HasPrefix(got, "(truncated)") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("fits untouched", func(t *testing.T) {
		got := Render(Input{InboxTag: "inbox", Text: "abc"}, 12)
		if got != "abc\n\n#inbox" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("fixed parts exceeding limit are never cut", func(t *testing.T) {
		got := Render(Input{
			InboxTag: "inbox", TodoTag: "todo", Text: "main",
			Actions: []string{strings.Repeat("a", 50)},
		}, 20)
		if !strings.Contains(got, strings.Repeat("a", 50)) || !strings.Contains(got, "#inbox #todo") {
			t.Errorf("fixed parts cut: %q", got)
		}
	})
}
