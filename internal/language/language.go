// Package language holds the pure language rules: normalising what
// speech-to-text reports to ISO-639-1 and deciding which language to ask
// for (research R3, R4).
package language

import (
	"maps"
	"strings"
	"sync"
)

// whisperCodes is Whisper's language list (openai/whisper tokenizer.py LANGUAGES)
// as ISO-639-1 code to English name. Whisper's non-ISO-639-1 entries ("haw",
// "yue") are deliberately left out, and its legacy "jw" is stored as "jv".
func whisperCodes() map[string]string {
	return map[string]string{
		"en": "english", "zh": "chinese", "de": "german", "es": "spanish",
		"ru": "russian", "ko": "korean", "fr": "french", "ja": "japanese",
		"pt": "portuguese", "tr": "turkish", "pl": "polish", "ca": "catalan",
		"nl": "dutch", "ar": "arabic", "sv": "swedish", "it": "italian",
		"id": "indonesian", "hi": "hindi", "fi": "finnish", "vi": "vietnamese",
		"he": "hebrew", "uk": "ukrainian", "el": "greek", "ms": "malay",
		"cs": "czech", "ro": "romanian", "da": "danish", "hu": "hungarian",
		"ta": "tamil", "no": "norwegian", "th": "thai", "ur": "urdu",
		"hr": "croatian", "bg": "bulgarian", "lt": "lithuanian", "la": "latin",
		"mi": "maori", "ml": "malayalam", "cy": "welsh", "sk": "slovak",
		"te": "telugu", "fa": "persian", "lv": "latvian", "bn": "bengali",
		"sr": "serbian", "az": "azerbaijani", "sl": "slovenian", "kn": "kannada",
		"et": "estonian", "mk": "macedonian", "br": "breton", "eu": "basque",
		"is": "icelandic", "hy": "armenian", "ne": "nepali", "mn": "mongolian",
		"bs": "bosnian", "kk": "kazakh", "sq": "albanian", "sw": "swahili",
		"gl": "galician", "mr": "marathi", "pa": "punjabi", "si": "sinhala",
		"km": "khmer", "sn": "shona", "yo": "yoruba", "so": "somali",
		"af": "afrikaans", "oc": "occitan", "ka": "georgian", "be": "belarusian",
		"tg": "tajik", "sd": "sindhi", "gu": "gujarati", "am": "amharic",
		"yi": "yiddish", "lo": "lao", "uz": "uzbek", "fo": "faroese",
		"ht": "haitian creole", "ps": "pashto", "tk": "turkmen", "nn": "nynorsk",
		"mt": "maltese", "sa": "sanskrit", "lb": "luxembourgish", "my": "myanmar",
		"bo": "tibetan", "tl": "tagalog", "mg": "malagasy", "as": "assamese",
		"tt": "tatar", "ln": "lingala", "ha": "hausa", "ba": "bashkir",
		"jv": "javanese", "su": "sundanese",
	}
}

// aliases are Whisper's extra names for the same languages.
func whisperAliases() map[string]string {
	return map[string]string{
		"burmese": "my", "valencian": "ca", "castilian": "es", "flemish": "nl",
		"haitian": "ht", "letzeburgesch": "lb", "moldavian": "ro",
		"moldovan": "ro", "panjabi": "pa", "pushto": "ps", "sinhalese": "si",
		"mandarin": "zh", "jw": "jv",
	}
}

// tables holds the lookups built from the embedded Whisper list.
type tables struct {
	codes map[string]string // ISO-639-1 code to name
	names map[string]string // lowercase name or alias to code
}

// lookup builds the lookups once on first use; a package-level cache is the
// simplest way to keep the embedded table immutable and cheap to query.
//
//nolint:gochecknoglobals // immutable lookup built once from the embedded table
var lookup = sync.OnceValue(func() tables {
	t := tables{codes: whisperCodes(), names: make(map[string]string)}
	for code, name := range t.codes {
		t.names[name] = code
	}

	maps.Copy(t.names, whisperAliases())

	return t
})

func clean(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Normalize maps a Whisper full name ("russian") or an ISO-639-1 code to the
// ISO-639-1 code, case-insensitively and ignoring surrounding whitespace.
// Anything else, including Whisper's non-ISO-639-1 languages ("haw", "yue"),
// reports ok=false.
func Normalize(raw string) (iso string, ok bool) {
	s := clean(raw)
	t := lookup()
	if _, known := t.codes[s]; known {
		return s, true
	}

	if code, known := t.names[s]; known {
		return code, true
	}

	return "", false
}

// canon returns the ISO code when s is recognised, else the cleaned input.
func canon(s string) string {
	if iso, ok := Normalize(s); ok {
		return iso
	}
	return clean(s)
}

// Plan says which language to pass to speech-to-text: exactly one configured
// source forces it; otherwise detection is left to the service (forced=false).
func Plan(sources []string) (language string, forced bool) {
	if len(sources) == 1 {
		return canon(sources[0]), true
	}
	return "", false
}

// Recheck decides, after a transcription, whether to transcribe again. With
// several sources and a reported language that is not among them, the answer
// is to retry once with the first source. Empty or single source lists, and a
// missing detected language, accept the result.
func Recheck(sources []string, detected string) (retranscribeWith string, ok bool) {
	if len(sources) < 2 || clean(detected) == "" {
		return "", false
	}
	got := canon(detected)
	for _, s := range sources {
		if canon(s) == got {
			return "", false
		}
	}

	return canon(sources[0]), true
}
