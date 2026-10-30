package digest

import (
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxSummaryRunes is the longest summary shown on a digest line.
const MaxSummaryRunes = 80

// TelegramMaxUnits is Telegram's message limit, in UTF-16 code units.
const TelegramMaxUnits = 4096

var tagPattern = regexp.MustCompile(`(^|\s)#[\p{L}\p{N}_/-]+`)

// Compose builds the morning digest. seen holds the names of notes already
// shown in an earlier digest; summaries maps a note name to its LLM summary.
// At most maxNotes note lines are listed (maxNotes <= 0 means no cap), new notes first,
// followed by "…and N more". The header and section counts describe the lines
// actually listed. It returns ok=false when there are no notes.
func Compose(notes []NoteSummary, seen map[string]bool, summaries map[string]string, maxNotes int) (string, bool) {
	if len(notes) == 0 {
		return "", false
	}
	fresh, earlier := partition(notes, seen)
	total := len(fresh) + len(earlier)
	fresh, earlier = capLines(fresh, earlier, maxNotes)
	more := total - len(fresh) - len(earlier)

	var b strings.Builder
	fmt.Fprintf(&b, "☀️ VoiceNotes — %d new, %d from earlier\n", len(fresh), len(earlier))
	if len(fresh) > 0 {
		b.WriteString("\n")
		writeLines(&b, fresh, summaries)
	}
	if len(earlier) > 0 {
		fmt.Fprintf(&b, "\nFrom earlier (%d)\n", len(earlier))
		writeLines(&b, earlier, summaries)
	}
	if more > 0 {
		fmt.Fprintf(&b, "…and %d more\n", more)
	}

	return strings.TrimRight(b.String(), "\n"), true
}

// listedNotes returns the notes Compose would list, in listing order (new
// first, then earlier, each oldest first, capped at maxNotes).
func listedNotes(notes []NoteSummary, seen map[string]bool, maxNotes int) []NoteSummary {
	fresh, earlier := partition(notes, seen)
	fresh, earlier = capLines(fresh, earlier, maxNotes)

	return append(fresh, earlier...)
}

// partition splits notes into new and earlier, each oldest first.
func partition(notes []NoteSummary, seen map[string]bool) ([]NoteSummary, []NoteSummary) {
	var fresh, earlier []NoteSummary
	for _, n := range notes {
		if seen[n.Name] {
			earlier = append(earlier, n)
		} else {
			fresh = append(fresh, n)
		}
	}
	byAge := func(a, b NoteSummary) int { return a.CreateTime.Compare(b.CreateTime) }
	slices.SortStableFunc(fresh, byAge)
	slices.SortStableFunc(earlier, byAge)

	return fresh, earlier
}

// capLines keeps at most maxNotes lines in total. New notes keep priority; the
// earlier section loses lines first. maxNotes <= 0 means no cap.
func capLines(fresh, earlier []NoteSummary, maxNotes int) ([]NoteSummary, []NoteSummary) {
	if maxNotes <= 0 {
		return fresh, earlier
	}
	fresh = fresh[:min(len(fresh), maxNotes)]
	earlier = earlier[:min(len(earlier), maxNotes-len(fresh))]

	return fresh, earlier
}

func writeLines(b *strings.Builder, notes []NoteSummary, summaries map[string]string) {
	for _, n := range notes {
		b.WriteString("• ")
		if n.HasTaskList {
			b.WriteString("☑ ")
		}
		b.WriteString(html.EscapeString(summaryFor(n.Content, summaries[n.Name])))
		fmt.Fprintf(b, " — <a href=\"%s\">open</a>\n", html.EscapeString(n.URL))
	}
}

// summaryFor returns the LLM summary when present, else the first non-empty
// line of content with tags stripped, truncated to MaxSummaryRunes.
func summaryFor(content, llm string) string {
	if s := strings.TrimSpace(llm); s != "" {
		return truncateRunes(strings.Join(strings.Fields(s), " "), MaxSummaryRunes)
	}
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.Join(strings.Fields(tagPattern.ReplaceAllString(line, " ")), " ")
		if line != "" {
			return truncateRunes(line, MaxSummaryRunes)
		}
	}

	return ""
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit-1]) + "…"
}

// SplitMessage splits text into parts of at most limit UTF-16 units, breaking
// at line boundaries. A single line longer than limit is cut by rune.
func SplitMessage(text string, limit int) []string {
	if text == "" {
		return nil
	}
	var parts []string
	var cur strings.Builder
	curUnits := 0
	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, cur.String())
			cur.Reset()
			curUnits = 0
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		lu := units(line)
		for lu > limit {
			flush()
			var head string
			head, line = cutUnits(line, limit)
			parts = append(parts, head)
			lu = units(line)
		}
		if cur.Len() > 0 && curUnits+1+lu > limit {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
			curUnits++
		}
		cur.WriteString(line)
		curUnits += lu
	}
	flush()

	return parts
}

// cutUnits splits s after at most limit UTF-16 units, at a rune boundary,
// always taking at least one rune so a tiny limit still makes progress. Byte
// offsets come from range, so invalid UTF-8 cannot mis-advance the cut.
func cutUnits(s string, limit int) (head, rest string) {
	cut, hu := 0, 0
	for i, r := range s {
		ru := utf16.RuneLen(r)
		if hu+ru > limit && i > 0 {
			break
		}
		_, w := utf8.DecodeRuneInString(s[i:])
		cut, hu = i+w, hu+ru
	}

	return s[:cut], s[cut:]
}

func units(s string) int { return len(utf16.Encode([]rune(s))) }
