package enrich

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func filesOpts() Options {
	o := opts()
	o.ExcerptRunes = 4000

	return o
}

func TestBuildFilesRequest(t *testing.T) {
	ex := []Excerpt{{Name: "a.pdf", Text: "first body"}, {Name: "b.png", Text: "second body"}}
	msgs := BuildFilesRequest(filesOpts(), "my caption", ex)
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("messages = %+v", msgs)
	}
	sys, user := msgs[0].Content, msgs[1].Content

	for _, w := range []string{
		"untrusted", "never instructions", "ignore", `"hint"`, "160", "2 lines", "in en",
		`"language"`, `"text"`, `"translated"`, `"tasks"`, `"tags"`, "caption only",
	} {
		if !strings.Contains(sys, w) {
			t.Errorf("system prompt lacks %q:\n%s", w, sys)
		}
	}
	for _, w := range []string{"my caption", "--- a.pdf (excerpt) ---\nfirst body", "--- b.png (excerpt) ---\nsecond body"} {
		if !strings.Contains(user, w) {
			t.Errorf("user content lacks %q:\n%s", w, user)
		}
	}
	if strings.Index(user, "my caption") > strings.Index(user, "a.pdf") ||
		strings.Index(user, "a.pdf") > strings.Index(user, "b.png") {
		t.Errorf("order wrong:\n%s", user)
	}
}

func TestBuildFilesRequestNoCaption(t *testing.T) {
	o := filesOpts()
	msgs := BuildFilesRequest(o, "  ", []Excerpt{{Name: "a.pdf", Text: "body"}})
	if strings.Contains(msgs[1].Content, "Caption") {
		t.Errorf("caption section present:\n%s", msgs[1].Content)
	}
	sys := msgs[0].Content
	for _, w := range []string{`"text"`, `"translated"`, `"tasks"`} {
		if !strings.Contains(sys, w+": ") || !strings.Contains(sys, "only when there is a caption") {
			t.Errorf("caption-only key %s not conditioned:\n%s", w, sys)
		}
	}
}

func TestBuildFilesRequestDisabledFunctions(t *testing.T) {
	o := filesOpts()
	o.Translate, o.Tidy, o.Tasks = false, false, false
	sys := BuildFilesRequest(o, "c", []Excerpt{{Name: "a", Text: "b"}})[0].Content
	for _, w := range []string{`"translated"`, `"tasks"`, `"tags"`} {
		if strings.Contains(sys, w) {
			t.Errorf("disabled key %s present:\n%s", w, sys)
		}
	}
	if !strings.Contains(sys, `"hint"`) {
		t.Errorf("hint missing:\n%s", sys)
	}
}

func TestExcerptBudget(t *testing.T) {
	long := strings.Repeat("x", 1000)
	tests := []struct {
		name   string
		budget int
		in     []Excerpt
		want   []int // runes per file in the user content
	}{
		{"within budget keeps all", 5000, []Excerpt{{"a", long}, {"b", "yy"}}, []int{1000, 2}},
		{"first file takes the rest", 1500, []Excerpt{{"a", long}, {"b", long}}, []int{1000, 500}},
		{"reserve of 200 per later file", 1000, []Excerpt{{"a", long}, {"b", long}, {"c", long}}, []int{600, 200, 200}},
		{"budget below the floors", 300, []Excerpt{{"a", long}, {"b", long}, {"c", long}}, []int{200, 100}},
		{"short file frees budget", 1000, []Excerpt{{"a", "zz"}, {"b", long}}, []int{2, 998}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boundExcerpts(tt.in, tt.budget)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d", len(got))
			}
			for i, e := range got {
				if n := utf8.RuneCountInString(e.Text); n != tt.want[i] {
					t.Errorf("file %d = %d runes, want %d", i, n, tt.want[i])
				}
				if e.Name != tt.in[i].Name {
					t.Errorf("file %d name = %q", i, e.Name)
				}
			}
		})
	}
}

func TestExcerptBudgetNeverExceeded(t *testing.T) {
	in := make([]Excerpt, 30)
	for i := range in {
		in[i] = Excerpt{Name: "f", Text: strings.Repeat("x", 1000)}
	}
	got := boundExcerpts(in, 4000)
	total := 0
	for _, e := range got {
		total += utf8.RuneCountInString(e.Text)
	}
	if total != 4000 {
		t.Errorf("total = %d, want 4000", total)
	}
	if len(got) >= len(in) {
		t.Errorf("files past the budget were not omitted: %d", len(got))
	}
	if len(boundExcerpts(in, 0)) != 0 {
		t.Error("zero budget must yield no excerpts")
	}
}

func TestExcerptBudgetCountsRunes(t *testing.T) {
	got := boundExcerpts([]Excerpt{{"a", strings.Repeat("я", 500)}}, 300)
	if n := utf8.RuneCountInString(got[0].Text); n != 300 {
		t.Errorf("runes = %d", n)
	}
}

func TestParseFiles(t *testing.T) {
	ok := `{"language":"de","hint":"Two scanned invoices","text":"Hello","translated":true,"tasks":["Pay it"],"tags":["Bills"]}`
	tests := []struct {
		name    string
		raw     string
		caption string
		detect  string
		mod     func(*Options)
		check   func(*testing.T, Result)
		wantErr error
	}{
		{
			name: "full", raw: ok, caption: "Hallo", detect: "de",
			check: func(t *testing.T, r Result) {
				t.Helper()
				if r.Hint != "Two scanned invoices" || r.Text != "Hello" || !r.Translated ||
					len(r.Tasks) != 1 || len(r.Tags) != 1 || r.Tags[0] != "bills" || r.Language != "de" {
					t.Errorf("result = %+v", r)
				}
			},
		},
		{
			name: "no caption ignores text, translated and tasks",
			raw:  ok, caption: "",
			check: func(t *testing.T, r Result) {
				t.Helper()
				if r.Text != "" || r.Translated || r.Tasks != nil || r.Hint == "" {
					t.Errorf("result = %+v", r)
				}
			},
		},
		{name: "hint missing", raw: `{"text":"x"}`, caption: "x", wantErr: ErrInvalidOutput},
		{name: "hint empty", raw: `{"hint":"  ","text":"x"}`, caption: "x", wantErr: ErrInvalidOutput},
		{
			name: "hint cut to 2 lines", raw: `{"hint":"one\ntwo\nthree"}`,
			check: func(t *testing.T, r Result) {
				t.Helper()
				if r.Hint != "one\ntwo" {
					t.Errorf("hint = %q", r.Hint)
				}
			},
		},
		{
			name: "hint cut to 160 runes", raw: `{"hint":"` + strings.Repeat("я", 200) + `"}`,
			check: func(t *testing.T, r Result) {
				t.Helper()
				if utf8.RuneCountInString(r.Hint) != 160 {
					t.Errorf("hint runes = %d", utf8.RuneCountInString(r.Hint))
				}
			},
		},
		{name: "caption text missing", raw: `{"hint":"h"}`, caption: "cap", wantErr: ErrInvalidOutput},
		{
			name:    "caption text too long",
			raw:     `{"hint":"h","text":"` + strings.Repeat("a", 100) + `"}`,
			caption: "ab",
			wantErr: ErrInvalidOutput,
		},
		{
			name:    "not translated",
			raw:     `{"hint":"h","text":"Hallo","translated":false}`,
			caption: "Hallo",
			detect:  "de",
			wantErr: ErrNotTranslated,
		},
		{name: "malformed", raw: `nope`, wantErr: ErrInvalidOutput},
		{
			name: "tasks disabled", raw: ok, caption: "Hallo", mod: func(o *Options) { o.Tasks = false },
			check: func(t *testing.T, r Result) {
				t.Helper()
				if r.Tasks != nil {
					t.Errorf("tasks = %v", r.Tasks)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := filesOpts()
			if tt.mod != nil {
				tt.mod(&o)
			}
			r, err := ParseFiles(tt.raw, tt.caption, tt.detect, o)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(t, r)
			}
		})
	}
}

func TestResultHintRoundTrip(t *testing.T) {
	s, err := Result{Hint: "h", Text: "t"}.Marshal()
	if err != nil || !strings.Contains(s, `"hint":"h"`) {
		t.Fatalf("marshal = %q, %v", s, err)
	}
	r, err := Unmarshal(s)
	if err != nil || r.Hint != "h" {
		t.Fatalf("unmarshal = %+v, %v", r, err)
	}
	if s, _ := (Result{Text: "t"}).Marshal(); strings.Contains(s, "hint") {
		t.Errorf("empty hint serialised: %s", s)
	}
}
