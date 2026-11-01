package pipeline

import (
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

const (
	// maxNameBytes bounds a sanitised user-supplied file name, extension
	// included.
	maxNameBytes = 100
	// maxExtBytes bounds a sanitised extension, dot included.
	maxExtBytes = 16

	extJPG = ".jpg"
	extMP4 = ".mp4"
)

// SanitizeFileName reduces a user-supplied file name to a safe base name:
// directory parts are dropped, control characters (NUL included) are
// removed, and the result is cut to at most 100 bytes on a rune boundary
// with the extension kept. It returns "" when nothing usable is left.
func SanitizeFileName(name string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = stripControl(name)
	name = strings.TrimSpace(name)
	if strings.Trim(name, ".") == "" {
		return ""
	}
	ext := SanitizeExt(path.Ext(name))
	base := strings.TrimSuffix(name, path.Ext(name))
	if limit := maxNameBytes - len(ext); len(base) > limit {
		base = cutRunes(base, limit)
	}
	if base == "" {
		return ""
	}

	return base + ext
}

// SanitizeExt makes a file extension (with its leading dot, as path.Ext
// returns it) safe for the file system: control characters and path
// separators are removed, and an over-long extension is dropped entirely.
func SanitizeExt(ext string) string {
	ext = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return -1
		}

		return r
	}, ext)
	if len(ext) > maxExtBytes || ext == "." {
		return ""
	}

	return ext
}

// AttachmentNames returns the Memos attachment file name of each file of one
// capture, in order. It depends only on its arguments, so a retry produces
// the same names, and names are unique within the capture: later duplicates
// get " (2)", " (3)" before the extension.
//
// Audio is voice-<local date><ext>. Any other file keeps its sanitised
// original name; without one it is photo-<date>-<n>.jpg, video-<date>-<n><ext>
// or file-<date>-<n><ext>, n being the file's 1-based place in the capture.
// A nil loc means UTC.
func AttachmentNames(receivedAt time.Time, loc *time.Location, files []capture.File) []string {
	if loc == nil {
		loc = time.UTC
	}
	date := receivedAt.In(loc).Format(time.DateOnly)
	used := make(map[string]bool, len(files))
	names := make([]string, len(files))
	for i := range files {
		names[i] = uniqueName(used, baseAttachmentName(date, i+1, &files[i]))
	}

	return names
}

func baseAttachmentName(date string, n int, f *capture.File) string {
	ext := SanitizeExt(path.Ext(f.LocalPath))
	if f.Kind == capture.FileAudio {
		if ext == "" {
			ext = extOgg
		}

		return "voice-" + date + ext
	}
	if name := SanitizeFileName(f.FileName); name != "" {
		return name
	}
	if own := SanitizeExt(path.Ext(f.FileName)); own != "" {
		ext = own
	}
	num := "-" + date + "-" + strconv.Itoa(n)
	if f.Kind == capture.FileImage {
		return "photo" + num + extJPG
	}
	if f.Kind == capture.FileVideo {
		if ext == "" {
			ext = extMP4
		}

		return "video" + num + ext
	}

	return "file" + num + ext
}

// uniqueName returns name, or name with " (2)", " (3)", ... before the
// extension, whichever is the first not yet used, and records it.
func uniqueName(used map[string]bool, name string) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	out := name
	for k := 2; used[out]; k++ {
		out = base + " (" + strconv.Itoa(k) + ")" + ext
	}
	used[out] = true

	return out
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}

		return r
	}, s)
}

// cutRunes shortens s to at most limit bytes without splitting a rune.
func cutRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}

	return s[:limit]
}
