// Package render builds the Memos note content from a processed capture.
// It is pure: no I/O, so every layout and truncation rule is unit tested.
//
// Layout (FR-022), sections separated by a blank line:
//
//	main text (transcript, or a placeholder naming the failure)
//	checklist ("- [ ] item" lines, when there are action items)
//	original quote ("*Original (xx):*" then "> " lines, when translated)
//	tags line (inbox tag, todo tag with action items, suggested tags)
package render

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// NoSpeech is the main text for an empty transcript.
	NoSpeech = "(no speech recognized)"
	// truncatedMark is appended to shortened text.
	truncatedMark = " (truncated)"
)

// Input is everything the renderer needs. Zero-valued optional fields render
// nothing, so later pipeline steps only fill more of them in.
type Input struct {
	// Text is the main text: the raw transcript now; tidied or translated
	// text once the LLM step exists.
	Text string
	// Failure is a short reason for a permanent step failure. When set, the
	// main text becomes a placeholder naming it and Text is ignored.
	Failure string
	// InboxTag and TodoTag are tag names without "#", from configuration.
	InboxTag string
	TodoTag  string
	// Actions are action items rendered as a checklist; any item adds TodoTag.
	Actions []string
	// Original is the untranslated transcript, set only when translated;
	// OriginalLang is its language code (for example "ru").
	Original     string
	OriginalLang string
	// SuggestedTags are extra tags without "#"; duplicates of the inbox and
	// to-do tags are dropped.
	SuggestedTags []string
}

// Render returns the note content, at most limit bytes where possible. The
// original quote is shortened first, then the main text, each marked
// "(truncated)" and never split inside a UTF-8 rune. The checklist and tags
// are never cut, so they may exceed limit on their own.
func Render(in Input, limit int) string {
	main := mainText(in)
	quote := in.Original
	tags := tagsLine(in)

	build := func(m, q string) string {
		parts := []string{m}
		if len(in.Actions) > 0 {
			parts = append(parts, checklist(in.Actions))
		}
		if q != "" {
			parts = append(parts, quoteBlock(in.OriginalLang, q))
		}

		return strings.Join(append(parts, tags), "\n\n")
	}

	full := build(main, quote)
	if len(full) <= limit {
		return full
	}
	if quote != "" {
		q := shrink(quote, func(s string) bool { return len(build(main, s)) <= limit })
		if q != "" {
			return build(main, q)
		}
		// Not even the "(truncated)" stub fits: omit the quote and give the
		// main text the freed room.
		quote = ""
		if m := build(main, ""); len(m) <= limit {
			return m
		}
	}
	m := shrink(main, func(s string) bool { return len(build(s, quote)) <= limit })
	if m == "" {
		m = strings.TrimSpace(truncatedMark) // nothing fits: keep the bare marker
	}

	return build(m, quote)
}

func mainText(in Input) string {
	if in.Failure != "" {
		return "(audio saved without text: " + in.Failure + ")"
	}
	if t := strings.TrimSpace(in.Text); t != "" {
		return t
	}

	return NoSpeech
}

func checklist(items []string) string {
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = "- [ ] " + it
	}
	return strings.Join(lines, "\n")
}

func quoteBlock(lang, text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return fmt.Sprintf("*Original (%s):*\n%s", lang, strings.Join(lines, "\n"))
}

func tagsLine(in Input) string {
	seen := map[string]bool{}
	var tags []string
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			tags = append(tags, "#"+t)
		}
	}
	add(in.InboxTag)
	if len(in.Actions) > 0 {
		add(in.TodoTag)
	}
	for _, t := range in.SuggestedTags {
		add(t)
	}

	return strings.Join(tags, " ")
}

// shrink returns the longest rune-aligned prefix of s plus the truncation mark
// for which fits holds, or "" when even an empty prefix does not fit.
func shrink(s string, fits func(string) bool) string {
	cand := func(n int) string {
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		return strings.TrimSpace(strings.TrimRight(s[:n], " \n\t") + truncatedMark)
	}
	lo, hi, best := 0, len(s)-1, ""
	for lo <= hi {
		mid := (lo + hi) / 2
		if c := cand(mid); fits(c) {
			best, lo = c, mid+1
		} else {
			hi = mid - 1
		}
	}

	return best
}
