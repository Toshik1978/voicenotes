// Package config loads and validates the service configuration from
// environment variables. It performs no I/O beyond reading the environment.
//
// Every validation error has the shape "config: <VAR>: <problem>" and never
// includes the offending value, because the value may be a secret.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the complete, validated service configuration.
type Config struct {
	// Telegram.
	BotToken     string  `env:"TELEGRAM_BOT_TOKEN"`
	AllowedUsers []int64 `env:"-"` // TELEGRAM_ALLOWED_USERS, comma-separated
	// DigestChatID is DIGEST_CHAT_ID, or the first allowed user when unset.
	DigestChatID int64 `env:"DIGEST_CHAT_ID"`

	// Speech-to-text.
	STTBaseURL        string        `env:"STT_BASE_URL"`
	STTAPIKey         string        `env:"STT_API_KEY"`
	STTModel          string        `env:"STT_MODEL"`
	STTResponseFormat string        `env:"STT_RESPONSE_FORMAT" envDefault:"verbose_json"`
	STTTimeout        time.Duration `env:"STT_TIMEOUT"         envDefault:"120s"`

	// Languages. An empty SourceLanguages means auto-detect.
	SourceLanguages []string `env:"-"` // SOURCE_LANGUAGES, comma-separated
	TargetLanguage  string   `env:"TARGET_LANGUAGE" envDefault:"en"`

	// LLM. LLMEnabled is derived: true when LLM_BASE_URL is set.
	LLMEnabled       bool          `env:"-"`
	LLMBaseURL       string        `env:"LLM_BASE_URL"`
	LLMAPIKey        string        `env:"LLM_API_KEY"`
	LLMModel         string        `env:"LLM_MODEL"`
	LLMTranslate     bool          `env:"LLM_TRANSLATE"      envDefault:"true"`
	LLMTidy          bool          `env:"LLM_TIDY"           envDefault:"true"`
	LLMTasks         bool          `env:"LLM_TASKS"          envDefault:"true"`
	LLMDigestSummary bool          `env:"LLM_DIGEST_SUMMARY" envDefault:"true"`
	LLMMaxTags       int           `env:"LLM_MAX_TAGS"       envDefault:"3"`
	LLMTimeout       time.Duration `env:"LLM_TIMEOUT"        envDefault:"60s"`
	// LLMTemperature is nil when LLM_TEMPERATURE is unset or empty: the
	// request then carries no temperature, because some models accept only
	// their own default.
	LLMTemperature *float64 `env:"LLM_TEMPERATURE"`

	// Memos. MemosPublicURL defaults to MemosURL.
	MemosURL          string `env:"MEMOS_URL"`
	MemosToken        string `env:"MEMOS_TOKEN"`
	MemosPublicURL    string `env:"MEMOS_PUBLIC_URL"`
	MemosAPIPrefix    string `env:"MEMOS_API_PREFIX"    envDefault:"auto"`
	MemosContentLimit int    `env:"MEMOS_CONTENT_LIMIT" envDefault:"8192"`
	InboxTag          string `env:"INBOX_TAG"           envDefault:"inbox"`
	TodoTag           string `env:"TODO_TAG"            envDefault:"todo"`

	// Digest.
	DigestEnabled  bool          `env:"DIGEST_ENABLED"   envDefault:"true"`
	DigestTime     string        `env:"DIGEST_TIME"      envDefault:"07:30"`
	DigestCatchup  time.Duration `env:"DIGEST_CATCHUP"   envDefault:"3h"`
	DigestMaxNotes int           `env:"DIGEST_MAX_NOTES" envDefault:"30"`

	// Time zone. TZ is the standard variable Go reads for time.Local; it is
	// read here only to validate it, because Go silently falls back to UTC on
	// an unknown zone. Location is the zone TZ names, or UTC when TZ is unset
	// or empty: the zone time.Local has in the image (which ships no
	// /etc/localtime), made explicit so a host's local zone never leaks in.
	// It drives the digest schedule and the local dates in attachment names.
	TZ       string         `env:"TZ"`
	Location *time.Location `env:"-"`

	// Runtime.
	DataDir                string        `env:"DATA_DIR"                 envDefault:"/data"`
	HTTPAddr               string        `env:"HTTP_ADDR"                envDefault:":8080"`
	HealthPendingThreshold time.Duration `env:"HEALTH_PENDING_THRESHOLD" envDefault:"30m"`
	RetryMaxInterval       time.Duration `env:"RETRY_MAX_INTERVAL"       envDefault:"5m"`
	LogLevel               string        `env:"LOG_LEVEL"                envDefault:"info"`
}

var (
	langRe       = regexp.MustCompile(`^[a-z]{2}$`)
	digestTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
)

// Load reads and validates the configuration from the process environment.
func Load() (Config, error) {
	return Parse(env.ToMap(os.Environ()))
}

// Parse builds and validates a Config from the given environment map.
// All problems found are reported together, joined into one error.
func Parse(environ map[string]string) (Config, error) {
	var c Config
	failed := map[string]bool{}
	var errs []error

	if err := env.ParseWithOptions(&c, env.Options{Environment: environ}); err != nil {
		names := envNames()
		var agg env.AggregateError
		if !errors.As(err, &agg) {
			agg = env.AggregateError{Errors: []error{err}}
		}
		for _, e := range agg.Errors {
			if pe, ok := errors.AsType[env.ParseError](e); ok {
				v := names[pe.Name]
				failed[v] = true
				errs = append(errs, fieldErr(v, "invalid value"))
				continue
			}
			errs = append(errs, fmt.Errorf("config: %w", e))
		}
	}

	v := validator{c: &c, failed: failed, errs: errs}
	v.validate(environ)
	if len(v.errs) > 0 {
		return Config{}, errors.Join(v.errs...)
	}

	return c, nil
}

type validator struct {
	c      *Config
	failed map[string]bool
	errs   []error
}

func (v *validator) fail(name, problem string) {
	if v.failed[name] {
		return
	}
	v.failed[name] = true
	v.errs = append(v.errs, fieldErr(name, problem))
}

func (v *validator) required(name, value string) {
	if strings.TrimSpace(value) == "" {
		v.fail(name, "is required")
	}
}

func (v *validator) url(name, value string) {
	if v.failed[name] {
		return
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.fail(name, "must be an http or https URL")
	}
}

func (v *validator) positive(name string, ok bool) {
	if !ok {
		v.fail(name, "must be positive")
	}
}

func (v *validator) validate(environ map[string]string) {
	v.validateTelegram(environ)
	v.validateSTT()
	v.validateLanguages(environ)
	v.validateLLM()
	v.validateMemos()
	v.validateDigest()
	v.validateRuntime()
}

func (v *validator) validateTelegram(environ map[string]string) {
	c := v.c

	v.required("TELEGRAM_BOT_TOKEN", c.BotToken)
	users := splitList(environ["TELEGRAM_ALLOWED_USERS"])
	if len(users) == 0 {
		v.fail("TELEGRAM_ALLOWED_USERS", "is required")
	}
	for _, u := range users {
		id, err := strconv.ParseInt(u, 10, 64)
		if err != nil || id == 0 {
			v.fail("TELEGRAM_ALLOWED_USERS", "must be comma-separated Telegram user IDs")
			break
		}
		c.AllowedUsers = append(c.AllowedUsers, id)
	}
	if c.DigestChatID == 0 && len(c.AllowedUsers) > 0 {
		c.DigestChatID = c.AllowedUsers[0]
	}
}

func (v *validator) validateSTT() {
	c := v.c
	v.required("STT_BASE_URL", c.STTBaseURL)
	v.url("STT_BASE_URL", c.STTBaseURL)
	v.required("STT_MODEL", c.STTModel)
	if c.STTResponseFormat != "verbose_json" && c.STTResponseFormat != "json" {
		v.fail("STT_RESPONSE_FORMAT", "must be verbose_json or json")
	}
	v.positive("STT_TIMEOUT", c.STTTimeout > 0 || v.failed["STT_TIMEOUT"])
}

func (v *validator) validateLanguages(environ map[string]string) {
	c := v.c
	c.SourceLanguages = splitList(environ["SOURCE_LANGUAGES"])
	for _, l := range c.SourceLanguages {
		if !langRe.MatchString(l) {
			v.fail("SOURCE_LANGUAGES", "must be comma-separated ISO-639-1 codes")
		}
	}
	if !langRe.MatchString(c.TargetLanguage) {
		v.fail("TARGET_LANGUAGE", "must be an ISO-639-1 code")
	}
}

func (v *validator) validateLLM() {
	c := v.c
	c.LLMEnabled = c.LLMBaseURL != ""
	if c.LLMEnabled {
		v.url("LLM_BASE_URL", c.LLMBaseURL)
		v.required("LLM_MODEL", c.LLMModel)
	}
	if c.LLMMaxTags < 0 {
		v.fail("LLM_MAX_TAGS", "must not be negative")
	}
	v.positive("LLM_TIMEOUT", c.LLMTimeout > 0)
	// Written this way round so NaN fails too.
	if t := c.LLMTemperature; t != nil && !(*t >= 0 && *t <= 2) {
		v.fail("LLM_TEMPERATURE", "must be between 0 and 2")
	}
}

func (v *validator) validateMemos() {
	c := v.c
	v.required("MEMOS_URL", c.MemosURL)
	v.url("MEMOS_URL", c.MemosURL)
	v.required("MEMOS_TOKEN", c.MemosToken)
	if c.MemosPublicURL == "" {
		c.MemosPublicURL = c.MemosURL
	}
	v.url("MEMOS_PUBLIC_URL", c.MemosPublicURL)
	switch c.MemosAPIPrefix {
	case "auto", "/api", "/api/v1":
	default:
		v.fail("MEMOS_API_PREFIX", "must be auto, /api or /api/v1")
	}
	v.positive("MEMOS_CONTENT_LIMIT", c.MemosContentLimit > 0)
}

func (v *validator) validateDigest() {
	c := v.c
	if !digestTimeRe.MatchString(c.DigestTime) {
		v.fail("DIGEST_TIME", "must be HH:MM")
	}
	v.positive("DIGEST_CATCHUP", c.DigestCatchup > 0)
	v.positive("DIGEST_MAX_NOTES", c.DigestMaxNotes > 0)
}

func (v *validator) validateRuntime() {
	c := v.c
	// Like Go's time.Local, accept an optional leading colon; unlike it,
	// never fall back to UTC on a zone that does not load.
	if loc, err := time.LoadLocation(strings.TrimPrefix(c.TZ, ":")); err != nil {
		v.fail("TZ", "must be a known IANA time zone")
	} else {
		c.Location = loc
	}
	v.positive("HEALTH_PENDING_THRESHOLD", c.HealthPendingThreshold > 0)
	v.positive("RETRY_MAX_INTERVAL", c.RetryMaxInterval > 0)
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		v.fail("LOG_LEVEL", "must be debug, info, warn or error")
	}
}

// splitList splits a comma-separated value, trimming blanks and empty items.
func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}

func fieldErr(name, problem string) error {
	return fmt.Errorf("config: %s: %s", name, problem)
}

// envNames maps Config field names to their environment variable names.
func envNames() map[string]string {
	t := reflect.TypeFor[Config]()
	m := make(map[string]string, t.NumField())
	for f := range t.Fields() {
		m[f.Name] = f.Tag.Get("env")
	}

	return m
}
