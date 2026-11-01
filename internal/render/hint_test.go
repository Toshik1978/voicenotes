package render

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFallbackHint(t *testing.T) {
	doc := func(name, text string) FileSummary {
		return FileSummary{Kind: KindDocument, Name: name, State: StateDone, Text: text}
	}
	photo := FileSummary{Kind: KindImage, Name: "p.jpg", State: StateDone}
	failed := func(kind, name, why string) FileSummary {
		return FileSummary{Kind: kind, Name: name, State: StateFailed, Failure: why}
	}
	tests := []struct {
		name  string
		files []FileSummary
		want  string
	}{
		{"no files", nil, ""},
		{"first two lines", []FileSummary{doc("a.pdf", "One\nTwo\nThree")}, "One\nTwo"},
		{"strips markdown noise", []FileSummary{doc("a.pdf",
			"# Title\n\n<!-- image -->\n| Name | Qty |\n|---|---|\n| Milk | 2 |")}, "Title\nName Qty"},
		{"skips files without text", []FileSummary{doc("a.pdf", "  \n"), doc("b.pdf", "Body")}, "Body"},
		{
			"placeholder document",
			[]FileSummary{{Kind: KindDocument, Name: "report.pdf", State: StateEmpty}},
			"📎 report.pdf — no text found",
		},
		{"empty photo", []FileSummary{{Kind: KindImage, State: StateEmpty}}, "📷 photo — no text found"},
		{
			"all empty batch",
			[]FileSummary{{Kind: KindImage, State: StateEmpty}, {Kind: KindImage, State: StateEmpty}},
			"📷 2 photos — no text found",
		},
		{
			"skipped means no reader",
			[]FileSummary{{Kind: KindImage, State: "skipped"}},
			"📷 photo",
		},
		{
			"some empty some failed",
			[]FileSummary{{Kind: KindImage, State: StateEmpty}, failed(KindImage, "b", "too big")},
			"📷 2 photos",
		},
		{"placeholder photo", []FileSummary{photo}, "📷 photo"},
		{"placeholder photos", []FileSummary{photo, photo, photo}, "📷 3 photos"},
		{"placeholder files", []FileSummary{photo, doc("a", ""), doc("b", ""), doc("c", "")}, "📎 4 files"},
		{
			"all failed reason",
			[]FileSummary{failed(KindDocument, "scan.pdf", "couldn't read in time")},
			"📎 scan.pdf — couldn't read in time",
		},
		{
			"all failed batch",
			[]FileSummary{failed(KindImage, "a", "too big"), failed(KindImage, "b", "too big")},
			"📷 2 photos — too big",
		},
		{"some failed no reason", []FileSummary{failed(KindImage, "a", "too big"), photo}, "📷 2 photos"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FallbackHint(tc.files); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFallbackHintCap(t *testing.T) {
	got := FallbackHint([]FileSummary{{Kind: KindDocument, State: StateDone, Text: strings.Repeat("я", 300)}})
	if n := utf8.RuneCountInString(got); n != hintMaxRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %d runes %q", n, got)
	}
}

func TestRenderFileNote(t *testing.T) {
	pdf := []FileSummary{{Kind: KindDocument, Name: "a.pdf", State: StateDone, Text: "Contract body\nmore"}}
	base := Input{InboxTag: "inbox", TodoTag: "todo", Files: pdf}
	tests := []struct {
		name string
		mod  func(Input) Input
		want string
	}{
		{"fallback hint only", func(i Input) Input { return i }, "Contract body\nmore\n\n#inbox"},
		{"llm hint wins", func(i Input) Input { i.Hint = "A contract"; return i }, "A contract\n\n#inbox"},
		{
			"caption first", func(i Input) Input { i.Caption = "Sign this"; i.Hint = "A contract"; return i },
			"Sign this\n\nA contract\n\n#inbox",
		},
		{"full layout", func(i Input) Input {
			i.Caption, i.Hint = "Sign", "H"
			i.Actions = []string{"sign"}
			i.Original, i.OriginalLang = "Подпиши", "ru"
			i.SuggestedTags = []string{"legal"}
			return i
		}, "Sign\n\nH\n\n- [ ] sign\n\n*Original (ru):*\n> Подпиши\n\n#inbox #todo #legal"},
		{"voice fields ignored", func(i Input) Input { i.Text = "ignored"; return i }, "Contract body\nmore\n\n#inbox"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Render(tc.mod(base), big); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// FR-125: the first non-empty line is the caption, otherwise the hint.
func TestRenderFileNoteFirstLine(t *testing.T) {
	first := func(s string) string {
		for l := range strings.SplitSeq(s, "\n") {
			if strings.TrimSpace(l) != "" {
				return l
			}
		}

		return ""
	}
	in := Input{InboxTag: "inbox", Hint: "The hint", Files: []FileSummary{{Kind: KindImage}}}
	if got := first(Render(in, big)); got != "The hint" {
		t.Fatalf("no caption: %q", got)
	}
	in.Caption = "My caption"
	if got := first(Render(in, big)); got != "My caption" {
		t.Fatalf("caption: %q", got)
	}
}

func TestRenderFileNoteTruncation(t *testing.T) {
	in := Input{
		InboxTag: "inbox", Hint: "Hint", Caption: strings.Repeat("слово ", 100),
		Files: []FileSummary{{Kind: KindImage}},
	}
	got := Render(in, 120)
	if len(got) > 120 || !strings.Contains(got, "(truncated)") || !utf8.ValidString(got) {
		t.Fatalf("bad truncation (%d bytes): %q", len(got), got)
	}
	if !strings.HasSuffix(got, "Hint\n\n#inbox") {
		t.Fatalf("hint and tags must stay intact: %q", got)
	}
	in.Caption = ""
	in.Hint = strings.Repeat("x", 300)
	got = Render(in, 100)
	if len(got) > 100 || !strings.Contains(got, "(truncated)") || !strings.HasSuffix(got, "#inbox") {
		t.Fatalf("hint truncation: %q", got)
	}
}

func TestStripImageMarkers(t *testing.T) {
	tests := map[string]string{
		"":                                 "",
		"<!-- image -->\n\n<!-- image -->": "",
		"<!-- image -->\n# Title\nbody\n":  "# Title\nbody",
		"plain":                            "plain",
	}
	for in, want := range tests {
		if got := StripImageMarkers(in); got != want {
			t.Errorf("StripImageMarkers(%q) = %q, want %q", in, got, want)
		}
	}
}
