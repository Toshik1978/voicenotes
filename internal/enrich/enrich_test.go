package enrich

import (
	"errors"
	"strings"
	"testing"
)

func opts() Options {
	return Options{
		Translate: true,
		Tidy:      true,
		Tasks:     true,
		Target:    "en",
		MaxTags:   3,
		InboxTag:  "inbox",
		TodoTag:   "todo",
	}
}

func TestBuildRequest(t *testing.T) {
	tests := []struct {
		name    string
		mod     func(*Options)
		want    []string
		notWant []string
	}{
		{
			"all",
			func(*Options) {},
			[]string{"Translate", "Tidy", "action items", `"tags"`, `"tasks"`, `"translated"`},
			nil,
		},
		{
			"translate only",
			func(o *Options) { o.Tidy, o.Tasks = false, false },
			[]string{"Translate", `"translated"`},
			[]string{"Tidy", `"tasks"`, `"tags"`},
		},
		{
			"tasks only",
			func(o *Options) { o.Translate, o.Tidy = false, false },
			[]string{`"tasks"`},
			[]string{"Translate", `"tags"`, `"translated"`},
		},
		{
			"tidy only",
			func(o *Options) { o.Translate, o.Tasks = false, false },
			[]string{"Tidy", `"tags"`, "at most 3"},
			[]string{`"tasks"`},
		},
		{
			"max tags zero omits tags",
			func(o *Options) { o.MaxTags = 0 },
			[]string{"Tidy", `"tasks"`},
			[]string{`"tags"`, "at most 0"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := opts()
			tc.mod(&o)
			msgs := BuildRequest(o, "hello world", "ru")
			if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
				t.Fatalf("bad shape: %+v", msgs)
			}
			for _, w := range tc.want {
				if !strings.Contains(msgs[0].Content, w) {
					t.Errorf("system prompt missing %q", w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(msgs[0].Content, w) {
					t.Errorf("system prompt has %q", w)
				}
			}
			for _, w := range []string{"hello world", "ru", "en"} {
				if !strings.Contains(msgs[1].Content, w) {
					t.Errorf("user content missing %q", w)
				}
			}
		})
	}
}

func TestParse(t *testing.T) {
	long := strings.Repeat("x", 201)
	tests := []struct {
		name    string
		raw     string
		input   string
		mod     func(*Options)
		want    Result
		invalid bool
	}{
		{
			name:  "full",
			raw:   `{"language":"ru","text":"Call the dentist.","translated":true,"tasks":["Call the dentist"],"tags":["garden","health"]}`,
			input: "Позвонить врачу",
			want: Result{
				Language:   "ru",
				Text:       "Call the dentist.",
				Translated: true,
				Tasks:      []string{"Call the dentist"},
				Tags:       []string{"garden", "health"},
			},
		},
		{
			name:  "wrapped in prose and fence",
			raw:   "Here you go:\n```json\n{\"text\":\"Hi there\"}\n```\nthanks {x}",
			input: "hi there",
			want:  Result{Text: "Hi there"},
		},
		{
			name:  "braces inside strings",
			raw:   `{"text":"a } b { c"}`,
			input: "a b c",
			want:  Result{Text: "a } b { c"},
		},
		{
			name:  "inbox and todo tags dropped, dupes removed",
			raw:   `{"text":"x","tags":["inbox","todo","a","a","b"]}`,
			input: "x",
			want:  Result{Text: "x", Tags: []string{"a", "b"}},
		},
		{
			name:  "disabled functions ignored",
			raw:   `{"text":"x","translated":true,"tasks":["t"],"tags":["a"]}`,
			input: "x",
			mod:   func(o *Options) { o.Translate, o.Tidy, o.Tasks = false, false, false },
			want:  Result{Text: "x"},
		},
		{
			name:  "tasks trimmed, blanks dropped",
			raw:   `{"text":"x","tasks":["  a  ",""," "]}`,
			input: "x",
			want:  Result{Text: "x", Tasks: []string{"a"}},
		},
		{
			name:  "text at 4x limit counts runes",
			raw:   `{"text":"яяяяяяяя"}`,
			input: "яя",
			want:  Result{Text: "яяяяяяяя"},
		},
		{
			name:  "200 runes task ok",
			raw:   `{"text":"x","tasks":["` + strings.Repeat("я", 200) + `"]}`,
			input: "x",
			want:  Result{Text: "x", Tasks: []string{strings.Repeat("я", 200)}},
		},
		{
			name:  "max tags zero ignores tags",
			raw:   `{"text":"x","tags":["a","BAD tag"]}`,
			input: "x",
			mod:   func(o *Options) { o.MaxTags = 0 },
			want:  Result{Text: "x"},
		},
		{name: "no json", raw: "sorry", input: "x", invalid: true},
		{name: "unterminated", raw: `{"text":"x"`, input: "x", invalid: true},
		{name: "empty text", raw: `{"text":"  "}`, input: "x", invalid: true},
		{name: "missing text", raw: `{"tags":[]}`, input: "x", invalid: true},
		{name: "text not string", raw: `{"text":5}`, input: "x", invalid: true},
		{name: "text too long", raw: `{"text":"123456789"}`, input: "ab", invalid: true},
		{
			name:    "too many tasks",
			raw:     `{"text":"x","tasks":["1","2","3","4","5","6","7","8","9","10","11"]}`,
			input:   "x",
			invalid: true,
		},
		{name: "task too long", raw: `{"text":"x","tasks":["` + long + `"]}`, input: "x", invalid: true},
		{name: "task not string", raw: `{"text":"x","tasks":[1]}`, input: "x", invalid: true},
		{
			name:  "tags normalised",
			raw:   `{"text":"x","tags":["#Work","  Garden Shed ","##ideas","Home\tOffice"]}`,
			input: "x",
			mod:   func(o *Options) { o.MaxTags = 5 },
			want:  Result{Text: "x", Tags: []string{"work", "garden-shed", "ideas", "home-office"}},
		},
		{
			name:  "invalid tags dropped, the rest kept",
			raw:   `{"text":"x","tasks":["t"],"tags":["a+b","","#","` + strings.Repeat("a", 33) + `","ok","дом"]}`,
			input: "x",
			want:  Result{Text: "x", Tasks: []string{"t"}, Tags: []string{"ok"}},
		},
		{
			name:  "reserved tags dropped after normalising",
			raw:   `{"text":"x","tags":["#Inbox","TODO","a"]}`,
			input: "x",
			want:  Result{Text: "x", Tags: []string{"a"}},
		},
		{
			name:  "dupes after normalising removed, order kept",
			raw:   `{"text":"x","tags":["b","#B","a","A"]}`,
			input: "x",
			want:  Result{Text: "x", Tags: []string{"b", "a"}},
		},
		{
			name:  "too many tags truncated",
			raw:   `{"text":"x","tags":["a","b","c","d"]}`,
			input: "x",
			want:  Result{Text: "x", Tags: []string{"a", "b", "c"}},
		},
		{
			name:  "dropped tags do not count",
			raw:   `{"text":"x","tags":["inbox","Bad Tag!","a","b","c"]}`,
			input: "x",
			want:  Result{Text: "x", Tags: []string{"a", "b", "c"}},
		},
		{
			name:  "tags not an array drops only the tags",
			raw:   `{"text":"x","tasks":["t"],"tags":"a, b"}`,
			input: "x",
			want:  Result{Text: "x", Tasks: []string{"t"}},
		},
		{
			name:  "non-string tag items dropped",
			raw:   `{"text":"x","tags":[1,"a",null,{"b":1},"b"]}`,
			input: "x",
			want:  Result{Text: "x", Tags: []string{"a", "b"}},
		},
		{
			name:  "null tags",
			raw:   `{"text":"x","tags":null}`,
			input: "x",
			want:  Result{Text: "x"},
		},
		{name: "language not string", raw: `{"text":"x","language":3}`, input: "x", invalid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := opts()
			if tc.mod != nil {
				tc.mod(&o)
			}
			got, err := Parse(tc.raw, tc.input, o)
			if tc.invalid {
				if !errors.Is(err, ErrInvalidOutput) {
					t.Fatalf("err = %v, want ErrInvalidOutput", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Language != tc.want.Language || got.Text != tc.want.Text || got.Translated != tc.want.Translated ||
				strings.Join(got.Tasks, "|") != strings.Join(tc.want.Tasks, "|") ||
				strings.Join(got.Tags, "|") != strings.Join(tc.want.Tags, "|") {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResultJSONRoundTrip(t *testing.T) {
	in := Result{Language: "ru", Text: "t", Translated: true, Tasks: []string{"a"}, Tags: []string{"b"}}
	s, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Unmarshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "t" || out.Language != "ru" || !out.Translated || out.Tasks[0] != "a" || out.Tags[0] != "b" {
		t.Fatalf("got %+v", out)
	}
	if _, err := Unmarshal("not json"); err == nil {
		t.Fatal("want error")
	}
}

func TestBuildSummaryRequest(t *testing.T) {
	notes := []string{"first note", strings.Repeat("я", 600)}
	msgs := BuildSummaryRequest(notes)
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("bad shape: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "80") || !strings.Contains(msgs[0].Content, `"summaries"`) {
		t.Errorf("system prompt: %q", msgs[0].Content)
	}
	u := msgs[1].Content
	if !strings.HasPrefix(u, `[{"id":"1","text":"first note"},{"id":"2","text":"`) {
		t.Errorf("user content: %q", u)
	}
	if strings.Count(u, "я") != 500 {
		t.Errorf("second text should be cut to 500 runes, got %d", strings.Count(u, "я"))
	}
}

func TestParseSummaries(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		n       int
		want    map[string]string
		invalid bool
	}{
		{"ok", `{"summaries":{"1":"One","2":"Two"}}`, 2, map[string]string{"1": "One", "2": "Two"}, false},
		{"prose around", "ok: {\"summaries\":{\"1\":\" One \"}} done", 1, map[string]string{"1": "One"}, false},
		{
			"drops empty, long, non-string, unknown",
			`{"summaries":{"1":"","2":"` + strings.Repeat("a", 81) + `","3":5,"4":"x","9":"y"}}`,
			4,
			map[string]string{"4": "x"},
			false,
		},
		{
			"80 runes ok",
			`{"summaries":{"1":"` + strings.Repeat("я", 80) + `"}}`,
			1,
			map[string]string{"1": strings.Repeat("я", 80)},
			false,
		},
		{"multi-line dropped", `{"summaries":{"1":"a\nb"}}`, 1, map[string]string{}, false},
		{"no json", "nope", 1, nil, true},
		{"no summaries key", `{"x":1}`, 1, nil, true},
		{"summaries not object", `{"summaries":["a"]}`, 1, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSummaries(tc.raw, tc.n)
			if tc.invalid {
				if !errors.Is(err, ErrInvalidOutput) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
