// Package enrich builds the LLM prompts and validates the LLM answers for the
// optional enrichment step (translation, tidy-up, tags, action items) and for
// the digest summaries. It is pure: no I/O. The chat call itself lives in
// internal/openai; Message mirrors its shape so the caller maps it field by
// field, which keeps this package free of that import.
//
// All length limits count runes, not bytes.
package enrich

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits from contracts/external-apis.md section 2.
const (
	maxTextFactor   = 4
	maxTasks        = 10
	maxTaskRunes    = 200
	MaxSummaryRunes = 80
	summaryInRunes  = 500
)

// ErrInvalidOutput marks an LLM answer that failed validation. The caller maps
// it onto capture.SkippedInvalidOutput.
var ErrInvalidOutput = errors.New("invalid LLM output")

var tagPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Message is one chat message; Role is "system" or "user".
type Message struct {
	Role    string
	Content string
}

// Options selects the enabled functions and the validation parameters.
type Options struct {
	Translate bool
	Tidy      bool
	Tasks     bool
	// Target is the target language code, for example "en".
	Target string
	// MaxTags bounds the suggested tags.
	MaxTags int
	// InboxTag and TodoTag are dropped from suggested tags.
	InboxTag string
	TodoTag  string
}

// Result is the validated per-capture LLM output. It is stored as
// enriched_json and consumed by the renderer.
type Result struct {
	Language   string   `json:"language,omitempty"`
	Text       string   `json:"text"`
	Translated bool     `json:"translated,omitempty"`
	Tasks      []string `json:"tasks,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

// Marshal encodes the result for storage in enriched_json.
func (r Result) Marshal() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode enriched result: %w", err)
	}

	return string(b), nil
}

// Unmarshal decodes a stored enriched_json value.
func Unmarshal(s string) (Result, error) {
	var r Result
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return Result{}, fmt.Errorf("decode enriched result: %w", err)
	}

	return r, nil
}

// BuildRequest returns the system and user messages for one capture. The
// system prompt covers only the enabled functions and the JSON shape; the user
// content holds the text, the detected language and the target language.
func BuildRequest(o Options, text, detectedLang string) []Message {
	var sys strings.Builder
	sys.WriteString(
		"You process a voice note transcript. Answer with one JSON object only, no prose and no code fences.\n",
	)
	shape := []string{`"language": detected ISO-639-1 code of the input`, `"text": string`}

	sys.WriteString("Functions:\n")
	if o.Translate {
		sys.WriteString("- Translate the text to the target language if it differs; otherwise keep it as is.\n")
	}
	if o.Tidy {
		sys.WriteString("- Tidy punctuation, casing and paragraphs without changing the meaning.\n")
	}
	if !o.Translate && !o.Tidy {
		sys.WriteString("- Return the text unchanged.\n")
	}
	if o.Translate {
		shape = append(shape, `"translated": true if you translated the text, else false`)
	}
	if o.Tidy && o.MaxTags > 0 {
		fmt.Fprintf(
			&sys,
			"- Suggest at most %d short topic tags: lowercase a-z, 0-9, _ or -, up to 32 characters, no #.\n",
			o.MaxTags,
		)
		shape = append(shape, `"tags": array of strings`)
	}
	if o.Tasks {
		sys.WriteString(
			"- Extract action items the speaker must do as short imperative sentences " +
				"(at most 10, each under 200 characters), in the language of \"text\".\n",
		)
		shape = append(shape, `"tasks": array of strings, empty if none`)
	}
	sys.WriteString("JSON keys:\n")
	for _, s := range shape {
		sys.WriteString("- " + s + "\n")
	}

	user := fmt.Sprintf("Detected language: %s\nTarget language: %s\nText:\n%s", detectedLang, o.Target, text)

	return []Message{{Role: "system", Content: sys.String()}, {Role: "user", Content: user}}
}

type rawResult struct {
	Language   *string         `json:"language"`
	Text       *string         `json:"text"`
	Translated *bool           `json:"translated"`
	Tasks      []string        `json:"tasks"`
	Tags       json.RawMessage `json:"tags"`
}

// Parse extracts the first JSON object from raw and validates it against the
// contract. input is the text that was sent; it bounds the answer length.
// Keys of disabled functions are ignored. Suggested tags are normalised
// leniently (see normaliseTags) and never fail the answer; any other violation
// yields ErrInvalidOutput.
func Parse(raw, input string, o Options) (Result, error) {
	var r rawResult
	if err := decodeFirstObject(raw, &r); err != nil {
		return Result{}, err
	}
	if r.Text == nil {
		return Result{}, invalid("text missing")
	}
	text := strings.TrimSpace(*r.Text)
	if text == "" {
		return Result{}, invalid("text empty")
	}
	if utf8.RuneCountInString(text) > maxTextFactor*utf8.RuneCountInString(input) {
		return Result{}, invalid("text too long")
	}

	return finish(r, text, o)
}

// finish applies the optional keys of the enabled functions.
func finish(r rawResult, text string, o Options) (Result, error) {
	res := Result{Text: text}
	if r.Language != nil {
		res.Language = strings.TrimSpace(*r.Language)
	}
	if o.Translate && r.Translated != nil {
		res.Translated = *r.Translated
	}
	if o.Tasks {
		tasks, err := cleanTasks(r.Tasks)
		if err != nil {
			return Result{}, err
		}
		res.Tasks = tasks
	}
	if o.Tidy && o.MaxTags > 0 {
		res.Tags = normaliseTags(r.Tags, o)
	}

	return res, nil
}

func cleanTasks(in []string) ([]string, error) {
	if len(in) > maxTasks {
		return nil, invalid("too many tasks")
	}
	var out []string
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if utf8.RuneCountInString(t) > maxTaskRunes {
			return nil, invalid("task too long")
		}
		out = append(out, t)
	}

	return out, nil
}

// normaliseTags turns the suggested tags into valid ones instead of rejecting
// the answer: each string is trimmed, loses its leading #s, is lowercased and
// has inner whitespace runs replaced by "-". Entries that still do not match
// the tag pattern, the inbox and todo tags (the renderer adds them) and
// duplicates are dropped, order is kept, and the list is cut to MaxTags.
// A tags value that is not an array yields no tags; non-string items are
// skipped.
func normaliseTags(raw json.RawMessage, o Options) []string {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []string
	for _, item := range items {
		var t string
		if json.Unmarshal(item, &t) != nil {
			continue // not a string (null decodes to "" and fails the pattern)
		}
		t = strings.Join(strings.Fields(strings.ToLower(strings.TrimLeft(strings.TrimSpace(t), "#"))), "-")
		if !tagPattern.MatchString(t) || t == o.InboxTag || t == o.TodoTag || slices.Contains(out, t) {
			continue
		}
		out = append(out, t)
		if len(out) == o.MaxTags {
			break
		}
	}

	return out
}

// BuildSummaryRequest returns the messages for one digest summaries call.
// notes are listed as numbered items "1".."n", each cut to 500 runes.
func BuildSummaryRequest(notes []string) []Message {
	type item struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	items := make([]item, len(notes))
	for i, n := range notes {
		items[i] = item{ID: strconv.Itoa(i + 1), Text: truncateRunes(n, summaryInRunes)}
	}
	b, err := json.Marshal(items)
	if err != nil { // unreachable: plain strings
		b = []byte("[]")
	}

	sys := fmt.Sprintf("You summarise notes for a morning digest. The user sends a JSON array of {\"id\", \"text\"}. "+
		"Answer with one JSON object only, no prose and no code fences: "+
		"{\"summaries\": {\"<id>\": \"<one-line summary>\"}}. "+
		"Each summary is a single line of at most %d characters, in the language of the note.", MaxSummaryRunes)

	return []Message{{Role: "system", Content: sys}, {Role: "user", Content: string(b)}}
}

// ParseSummaries extracts {"summaries":{"<n>":"..."}} from raw and returns the
// valid entries for ids "1".."n". Entries that are unknown, not strings, empty,
// multi-line or longer than 80 runes are dropped; the caller falls back per
// line. A body without a summaries object is ErrInvalidOutput.
func ParseSummaries(raw string, n int) (map[string]string, error) {
	var r struct {
		Summaries map[string]json.RawMessage `json:"summaries"`
	}
	if err := decodeFirstObject(raw, &r); err != nil {
		return nil, err
	}
	if r.Summaries == nil {
		return nil, invalid("summaries missing")
	}
	out := make(map[string]string, len(r.Summaries))
	for k, v := range r.Summaries {
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue
		}
		s = strings.TrimSpace(s)
		if !knownID(k, n) || s == "" || strings.ContainsAny(s, "\r\n") || utf8.RuneCountInString(s) > MaxSummaryRunes {
			continue
		}
		out[k] = s
	}

	return out, nil
}

func knownID(k string, n int) bool {
	for i := 1; i <= n; i++ {
		if k == strconv.Itoa(i) {
			return true
		}
	}

	return false
}

// decodeFirstObject decodes the first JSON object found in raw into v.
func decodeFirstObject(raw string, v any) error {
	i := strings.IndexByte(raw, '{')
	if i < 0 {
		return invalid("no JSON object")
	}
	if err := json.NewDecoder(strings.NewReader(raw[i:])).Decode(v); err != nil {
		return invalid("malformed JSON")
	}

	return nil
}

func invalid(why string) error { return fmt.Errorf("%w: %s", ErrInvalidOutput, why) }

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}

	return string([]rune(s)[:n])
}
