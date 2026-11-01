package render

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// File kinds and states as the renderer sees them. The values mirror
// capture.FileKind and capture.FileState so callers convert with a plain
// string conversion, and render stays independent of other packages.
const (
	KindImage    = "image"
	KindDocument = "document"

	StateDone   = "done"
	StateEmpty  = "empty"
	StateFailed = "failed"
)

// hintMaxRunes caps the fallback hint before the ellipsis.
const hintMaxRunes = 160

// imageMarker is the placeholder docling leaves where a picture was.
const imageMarker = "<!-- image -->"

// StripImageMarkers removes docling's picture placeholders from text and trims
// the result, so text that held only pictures comes back empty.
func StripImageMarkers(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, imageMarker, ""))
}

// FileSummary is what the renderer needs to know about one kept file.
type FileSummary struct {
	// Kind is KindImage, KindDocument or another kind (treated as a file).
	Kind string
	// Name is the original file name; may be empty.
	Name string
	// State is the extraction state (StateDone, StateFailed, ...).
	State string
	// Failure is the short reason when State is StateFailed.
	Failure string
	// Text is the extracted text, empty when none.
	Text string
}

// FallbackHint builds the hint used when the LLM step is off or failed: the
// first two cleaned lines of the first extracted text, capped at 160 runes.
// Without extracted text it returns a placeholder naming the item, followed by
// " — <reason>" when every file failed or " — no text found" when every file
// came back empty. It returns "" for no files.
func FallbackHint(files []FileSummary) string {
	if len(files) == 0 {
		return ""
	}
	for _, f := range files {
		if lines := cleanLines(f.Text); len(lines) > 0 {
			if len(lines) > 2 {
				lines = lines[:2]
			}
			return capRunes(strings.Join(lines, "\n"), hintMaxRunes)
		}
	}

	return placeholder(files)
}

func placeholder(files []FileSummary) string {
	return itemPlaceholder(files) + failureSuffix(files)
}

func itemPlaceholder(files []FileSummary) string {
	photos := 0
	for _, f := range files {
		if f.Kind == KindImage {
			photos++
		}
	}

	switch {
	case len(files) == 1 && photos == 1:
		return "📷 photo"
	case len(files) == 1 && files[0].Name != "":
		return "📎 " + files[0].Name
	case len(files) == 1:
		return "📎 file"
	case photos == len(files):
		return fmt.Sprintf("📷 %d photos", len(files))
	default:
		return fmt.Sprintf("📎 %d files", len(files))
	}
}

// failureSuffix is " — <reason>" when every file failed with a reason, or
// " — no text found" when every file came back empty. Empty means a reader ran
// and found nothing; a file no reader was configured for is skipped instead,
// so it keeps the plain placeholder (FR-129).
func failureSuffix(files []FileSummary) string {
	if allInState(files, StateEmpty) {
		return " — no text found"
	}
	reason := ""
	for _, f := range files {
		if f.State != StateFailed {
			return ""
		}
		if reason == "" {
			reason = f.Failure
		}
	}
	if reason == "" {
		return ""
	}

	return " — " + reason
}

func allInState(files []FileSummary, state string) bool {
	for _, f := range files {
		if f.State != state {
			return false
		}
	}

	return len(files) > 0
}

// cleanLines drops markdown noise and blank lines from extracted text.
func cleanLines(text string) []string {
	var out []string
	for raw := range strings.SplitSeq(text, "\n") {
		l := strings.ReplaceAll(raw, imageMarker, "")
		l = strings.TrimSpace(l)
		l = strings.TrimLeft(l, "# ")
		if strings.Contains(l, "|") {
			l = strings.Join(strings.Fields(strings.ReplaceAll(l, "|", " ")), " ")
		}
		if strings.Trim(l, "-: ") == "" {
			continue // blank, or a table separator row such as |---|---|
		}
		out = append(out, strings.Join(strings.Fields(l), " "))
	}

	return out
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)

	return strings.TrimRight(string(r[:n]), " \n") + "…"
}
