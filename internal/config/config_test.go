package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// secretValue is planted in every secret variable; no error may contain it.
const secretValue = "S3CR3T-DO-NOT-LEAK"

func baseEnv() map[string]string {
	return map[string]string{
		"TELEGRAM_BOT_TOKEN":     "123456:FAKE-TOKEN",
		"TELEGRAM_ALLOWED_USERS": "111, 222",
		"STT_BASE_URL":           "http://stt.example.org:8000/v1",
		"STT_MODEL":              "whisper-1",
		"MEMOS_URL":              "http://memos.example.org:5230",
		"MEMOS_TOKEN":            "memos_pat_FAKE",
	}
}

func with(kv ...string) map[string]string {
	m := baseEnv()
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
			continue
		}
		m[kv[i]] = kv[i+1]
	}

	return m
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse(baseEnv())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"DigestChatID", c.DigestChatID, int64(111)},
		{"AllowedUsers len", len(c.AllowedUsers), 2},
		{"STTResponseFormat", c.STTResponseFormat, "verbose_json"},
		{"STTTimeout", c.STTTimeout, 120 * time.Second},
		{"SourceLanguages", len(c.SourceLanguages), 0},
		{"TargetLanguage", c.TargetLanguage, "en"},
		{"LLMEnabled", c.LLMEnabled, false},
		{"LLMTranslate", c.LLMTranslate, true},
		{"LLMTidy", c.LLMTidy, true},
		{"LLMTasks", c.LLMTasks, true},
		{"LLMDigestSummary", c.LLMDigestSummary, true},
		{"LLMMaxTags", c.LLMMaxTags, 3},
		{"LLMTimeout", c.LLMTimeout, 60 * time.Second},
		{"MemosPublicURL", c.MemosPublicURL, "http://memos.example.org:5230"},
		{"MemosAPIPrefix", c.MemosAPIPrefix, "auto"},
		{"MemosContentLimit", c.MemosContentLimit, 8192},
		{"InboxTag", c.InboxTag, "inbox"},
		{"TodoTag", c.TodoTag, "todo"},
		{"DigestEnabled", c.DigestEnabled, true},
		{"DigestTime", c.DigestTime, "07:30"},
		{"TZ", c.TZ, ""},
		{"Location", c.Location, time.UTC},
		{"DigestCatchup", c.DigestCatchup, 3 * time.Hour},
		{"DigestMaxNotes", c.DigestMaxNotes, 30},
		{"DataDir", c.DataDir, "/data"},
		{"HTTPAddr", c.HTTPAddr, ":8080"},
		{"HealthPendingThreshold", c.HealthPendingThreshold, 30 * time.Minute},
		{"RetryMaxInterval", c.RetryMaxInterval, 5 * time.Minute},
		{"LogLevel", c.LogLevel, "info"},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
	if c.AllowedUsers[0] != 111 || c.AllowedUsers[1] != 222 {
		t.Errorf("AllowedUsers = %v", c.AllowedUsers)
	}
}

func TestParseOverridesAndDerived(t *testing.T) {
	c, err := Parse(with(
		"DIGEST_CHAT_ID", "-1001234567890",
		"SOURCE_LANGUAGES", "ru, de",
		"TARGET_LANGUAGE", "de",
		"LLM_BASE_URL", "http://llm.example.org/v1",
		"LLM_MODEL", "alias",
		"LLM_TIDY", "false",
		"MEMOS_PUBLIC_URL", "https://memos.example.org",
		"MEMOS_API_PREFIX", "/api/v1",
		"TZ", "Europe/Berlin",
		"DIGEST_TIME", "06:05",
		"STT_RESPONSE_FORMAT", "json",
		"LOG_LEVEL", "debug",
	))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.DigestChatID != -1001234567890 {
		t.Errorf("DigestChatID = %d", c.DigestChatID)
	}
	if len(c.SourceLanguages) != 2 || c.SourceLanguages[0] != "ru" || c.SourceLanguages[1] != "de" {
		t.Errorf("SourceLanguages = %v", c.SourceLanguages)
	}
	if !c.LLMEnabled || c.LLMTidy || c.LLMModel != "alias" {
		t.Errorf("LLM = %v %v %q", c.LLMEnabled, c.LLMTidy, c.LLMModel)
	}
	if c.MemosPublicURL != "https://memos.example.org" || c.MemosAPIPrefix != "/api/v1" {
		t.Errorf("memos = %q %q", c.MemosPublicURL, c.MemosAPIPrefix)
	}
	if c.Location.String() != "Europe/Berlin" {
		t.Errorf("Location = %v", c.Location)
	}
	if c.STTResponseFormat != "json" || c.LogLevel != "debug" {
		t.Errorf("format/level = %q %q", c.STTResponseFormat, c.LogLevel)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing bot token", with("TELEGRAM_BOT_TOKEN", "-"), "config: TELEGRAM_BOT_TOKEN: "},
		{"missing users", with("TELEGRAM_ALLOWED_USERS", "-"), "config: TELEGRAM_ALLOWED_USERS: "},
		{"bad user id", with("TELEGRAM_ALLOWED_USERS", "1,abc"), "config: TELEGRAM_ALLOWED_USERS: "},
		{"bad digest chat", with("DIGEST_CHAT_ID", "x"), "config: DIGEST_CHAT_ID: "},
		{"missing stt url", with("STT_BASE_URL", "-"), "config: STT_BASE_URL: "},
		{"bad stt url", with("STT_BASE_URL", "not a url"), "config: STT_BASE_URL: "},
		{"missing stt model", with("STT_MODEL", "-"), "config: STT_MODEL: "},
		{"bad stt format", with("STT_RESPONSE_FORMAT", "xml"), "config: STT_RESPONSE_FORMAT: "},
		{"bad stt timeout", with("STT_TIMEOUT", "soon"), "config: STT_TIMEOUT: "},
		{"bad source language", with("SOURCE_LANGUAGES", "ru,english"), "config: SOURCE_LANGUAGES: "},
		{"bad target language", with("TARGET_LANGUAGE", "EN"), "config: TARGET_LANGUAGE: "},
		{"llm without model", with("LLM_BASE_URL", "http://llm.example.org/v1"), "config: LLM_MODEL: "},
		{"bad llm url", with("LLM_BASE_URL", "nope", "LLM_MODEL", "m"), "config: LLM_BASE_URL: "},
		{"bad max tags", with("LLM_MAX_TAGS", "-1"), "config: LLM_MAX_TAGS: "},
		{"bad bool", with("LLM_TIDY", "maybe"), "config: LLM_TIDY: "},
		{"missing memos url", with("MEMOS_URL", "-"), "config: MEMOS_URL: "},
		{"missing memos token", with("MEMOS_TOKEN", "-"), "config: MEMOS_TOKEN: "},
		{"bad public url", with("MEMOS_PUBLIC_URL", "::"), "config: MEMOS_PUBLIC_URL: "},
		{"bad api prefix", with("MEMOS_API_PREFIX", "/v2"), "config: MEMOS_API_PREFIX: "},
		{"bad content limit", with("MEMOS_CONTENT_LIMIT", "0"), "config: MEMOS_CONTENT_LIMIT: "},
		{"bad digest time", with("DIGEST_TIME", "7:30"), "config: DIGEST_TIME: "},
		{"digest time range", with("DIGEST_TIME", "25:00"), "config: DIGEST_TIME: "},
		{"bad tz", with("TZ", "Mars/Base"), "config: TZ: "},
		{"tz path", with("TZ", "/usr/share/zoneinfo/Europe/Berlin"), "config: TZ: "},
		{"bad max notes", with("DIGEST_MAX_NOTES", "0"), "config: DIGEST_MAX_NOTES: "},
		{"bad log level", with("LOG_LEVEL", "loud"), "config: LOG_LEVEL: "},
		{"non-positive interval", with("RETRY_MAX_INTERVAL", "0s"), "config: RETRY_MAX_INTERVAL: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.env)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q lacks %q", err, tt.want)
			}
		})
	}
}

func TestParseReportsAllProblems(t *testing.T) {
	_, err := Parse(with("STT_MODEL", "-", "MEMOS_URL", "-"))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, v := range []string{"STT_MODEL", "MEMOS_URL"} {
		if !strings.Contains(err.Error(), "config: "+v+": ") {
			t.Errorf("error %q lacks %s", err, v)
		}
	}
}

func TestErrorsNeverContainValues(t *testing.T) {
	bad := map[string]string{
		"TELEGRAM_BOT_TOKEN":     secretValue,
		"TELEGRAM_ALLOWED_USERS": secretValue,
		"STT_BASE_URL":           secretValue,
		"STT_API_KEY":            secretValue,
		"STT_MODEL":              "m",
		"STT_RESPONSE_FORMAT":    secretValue,
		"STT_TIMEOUT":            secretValue,
		"SOURCE_LANGUAGES":       secretValue,
		"TARGET_LANGUAGE":        secretValue,
		"LLM_BASE_URL":           secretValue,
		"LLM_API_KEY":            secretValue,
		"LLM_TIDY":               secretValue,
		"LLM_MAX_TAGS":           secretValue,
		"MEMOS_URL":              secretValue,
		"MEMOS_TOKEN":            secretValue,
		"MEMOS_PUBLIC_URL":       secretValue,
		"MEMOS_API_PREFIX":       secretValue,
		"MEMOS_CONTENT_LIMIT":    secretValue,
		"DIGEST_CHAT_ID":         secretValue,
		"DIGEST_TIME":            secretValue,
		"TZ":                     secretValue,
		"DIGEST_MAX_NOTES":       secretValue,
		"LOG_LEVEL":              secretValue,
	}
	_, err := Parse(bad)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("error leaks a value: %v", err)
	}
	if !strings.Contains(err.Error(), "config: TELEGRAM_ALLOWED_USERS: ") {
		t.Errorf("error lacks variable name: %v", err)
	}
}

func TestLoadReadsProcessEnvironment(t *testing.T) {
	for k, v := range baseEnv() {
		t.Setenv(k, v)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.STTModel != "whisper-1" {
		t.Errorf("STTModel = %q", c.STTModel)
	}
	t.Setenv("STT_MODEL", "")
	if _, err := Load(); err == nil {
		t.Error("expected error for empty required variable")
	}
}

func TestErrorUnwrap(t *testing.T) {
	_, err := Parse(with("STT_MODEL", "-"))
	var ve interface{ Unwrap() []error }
	if !errors.As(err, &ve) {
		t.Fatalf("error %T does not unwrap to a list", err)
	}
}

func TestParseTZ(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unset is UTC", baseEnv(), "UTC"},
		{"empty is UTC", with("TZ", ""), "UTC"},
		{"explicit UTC", with("TZ", "UTC"), "UTC"},
		{"leading colon", with("TZ", ":Asia/Tokyo"), "Asia/Tokyo"},
		{"zone", with("TZ", "America/New_York"), "America/New_York"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse(tt.env)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if c.Location.String() != tt.want {
				t.Errorf("Location = %v, want %s", c.Location, tt.want)
			}
		})
	}
}
