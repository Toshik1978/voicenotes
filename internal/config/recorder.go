package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/caarlos0/env/v11"
)

// recorderBytesPerSecond is the 16 kHz, 16-bit mono PCM rate the recorder
// captures at; it sizes a maximum-length recording.
const recorderBytesPerSecond = 32000

var recorderNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RecorderConfig is the complete, validated configuration of the
// voicenotes-mic recorder. It carries none of the service's settings.
type RecorderConfig struct {
	ServerURL    string `env:"MIC_SERVER_URL"`
	UploadToken  string `env:"MIC_UPLOAD_TOKEN"`
	RecorderName string `env:"MIC_RECORDER_NAME"`
	// CaptureDevice and InputDevice are both empty (auto-discovery) or both set.
	CaptureDevice string `env:"MIC_CAPTURE_DEVICE"`
	InputDevice   string `env:"MIC_INPUT_DEVICE"`

	Debounce  time.Duration `env:"MIC_DEBOUNCE"   envDefault:"500ms"`
	MinLength time.Duration `env:"MIC_MIN_LENGTH" envDefault:"2s"`
	MaxLength time.Duration `env:"MIC_MAX_LENGTH" envDefault:"10m"`

	// SpoolDir defaults to $STATE_DIRECTORY, else /var/lib/voicenotes-mic.
	SpoolDir     string `env:"MIC_SPOOL_DIR"`
	SpoolMinFree int64  `env:"MIC_SPOOL_MIN_FREE" envDefault:"268435456"`

	CheckinInterval  time.Duration `env:"MIC_CHECKIN_INTERVAL"   envDefault:"1m"`
	RetryMaxInterval time.Duration `env:"MIC_RETRY_MAX_INTERVAL" envDefault:"5m"`
	PendingThreshold time.Duration `env:"MIC_PENDING_THRESHOLD"  envDefault:"30m"`

	HTTPAddr string `env:"MIC_HTTP_ADDR" envDefault:"127.0.0.1:8081"`
	LogLevel string `env:"LOG_LEVEL"     envDefault:"info"`
}

// ParseRecorder builds and validates a RecorderConfig from environ, a list of
// KEY=value entries like os.Environ. It never reads the process environment.
// All problems found are reported together, joined into one error.
func ParseRecorder(environ []string) (RecorderConfig, error) {
	return parseRecorder(environ, os.Hostname)
}

// parseRecorder is ParseRecorder with the hostname lookup injected.
func parseRecorder(environ []string, hostname func() (string, error)) (RecorderConfig, error) {
	m := env.ToMap(environ)
	r := recorderValidator{failed: map[string]bool{}}

	if err := env.ParseWithOptions(&r.c, env.Options{Environment: m}); err != nil {
		names := recorderEnvNames()
		var agg env.AggregateError
		if !errors.As(err, &agg) {
			agg = env.AggregateError{Errors: []error{err}}
		}
		for _, e := range agg.Errors {
			if pe, ok := errors.AsType[env.ParseError](e); ok {
				r.fail(names[pe.Name], "invalid value")
				continue
			}
			r.errs = append(r.errs, fmt.Errorf("config: %w", e))
		}
	}

	r.validateRemote()
	r.validateName(hostname)
	r.validateDevices()
	r.validateRecording()
	r.validateSpool(m["STATE_DIRECTORY"])
	r.validateRuntime()

	if len(r.errs) > 0 {
		return RecorderConfig{}, errors.Join(r.errs...)
	}

	return r.c, nil
}

type recorderValidator struct {
	c      RecorderConfig
	failed map[string]bool
	errs   []error
}

func (r *recorderValidator) fail(name, problem string) {
	if !r.failed[name] {
		r.failed[name] = true
		r.errs = append(r.errs, fieldErr(name, problem))
	}
}

func (r *recorderValidator) inRange(name string, d, lo, hi time.Duration, bounds string) {
	if !r.failed[name] && (d < lo || d > hi) {
		r.fail(name, "must be between "+bounds)
	}
}

func (r *recorderValidator) validateRemote() {
	c := r.c
	if strings.TrimSpace(c.ServerURL) == "" {
		r.fail("MIC_SERVER_URL", "is required")
	} else if u, err := url.Parse(c.ServerURL); err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		r.fail("MIC_SERVER_URL", "must be an http or https URL")
	}
	switch {
	case strings.TrimSpace(c.UploadToken) == "":
		r.fail("MIC_UPLOAD_TOKEN", "is required")
	case utf8.RuneCountInString(c.UploadToken) < 32:
		r.fail("MIC_UPLOAD_TOKEN", "must be at least 32 characters")
	}
}

func (r *recorderValidator) validateName(hostname func() (string, error)) {
	if r.c.RecorderName == "" {
		h, err := hostname()
		if err != nil {
			r.fail("MIC_RECORDER_NAME", "is unset and the hostname is unavailable")
		}
		r.c.RecorderName = h
	}
	if !r.failed["MIC_RECORDER_NAME"] && !recorderNameRe.MatchString(r.c.RecorderName) {
		r.fail("MIC_RECORDER_NAME", "must be 1 to 64 characters of A-Z, a-z, 0-9, '.', '_' or '-'")
	}
}

func (r *recorderValidator) validateDevices() {
	c := r.c
	switch {
	case c.CaptureDevice != "" && c.InputDevice == "":
		r.fail("MIC_INPUT_DEVICE", "is required when MIC_CAPTURE_DEVICE is set")
	case c.InputDevice != "" && c.CaptureDevice == "":
		r.fail("MIC_CAPTURE_DEVICE", "is required when MIC_INPUT_DEVICE is set")
	}
}

func (r *recorderValidator) validateRecording() {
	c := r.c
	r.inRange("MIC_DEBOUNCE", c.Debounce, 0, 5*time.Second, "0 and 5s")
	r.inRange("MIC_MIN_LENGTH", c.MinLength, 0, time.Minute, "0 and 60s")
	r.inRange("MIC_MAX_LENGTH", c.MaxLength, 10*time.Second, 12*time.Minute, "10s and 12m")
	if !r.failed["MIC_MIN_LENGTH"] && !r.failed["MIC_MAX_LENGTH"] && c.MinLength >= c.MaxLength {
		r.fail("MIC_MIN_LENGTH", "must be shorter than MIC_MAX_LENGTH")
	}
}

func (r *recorderValidator) validateSpool(stateDir string) {
	c := &r.c
	if c.SpoolDir == "" {
		c.SpoolDir = stateDir
	}
	if c.SpoolDir == "" {
		c.SpoolDir = "/var/lib/voicenotes-mic"
	}
	if !r.failed["MIC_SPOOL_MIN_FREE"] && !r.failed["MIC_MAX_LENGTH"] {
		maxBytes := int64(c.MaxLength/time.Second) * recorderBytesPerSecond
		if c.SpoolMinFree < 2*maxBytes {
			r.fail("MIC_SPOOL_MIN_FREE", "must be at least twice the size of a maximum-length recording")
		}
	}
}

func (r *recorderValidator) validateRuntime() {
	c := r.c
	r.inRange("MIC_CHECKIN_INTERVAL", c.CheckinInterval, 10*time.Second, 10*time.Minute, "10s and 10m")
	r.inRange("MIC_RETRY_MAX_INTERVAL", c.RetryMaxInterval, 10*time.Second, time.Hour, "10s and 1h")
	if !r.failed["MIC_PENDING_THRESHOLD"] && c.PendingThreshold <= 0 {
		r.fail("MIC_PENDING_THRESHOLD", "must be positive")
	}
	if strings.TrimSpace(c.HTTPAddr) == "" {
		r.fail("MIC_HTTP_ADDR", "must not be empty")
	}
	if !validLogLevel(c.LogLevel) {
		r.fail("LOG_LEVEL", "must be debug, info, warn or error")
	}
}

func validLogLevel(s string) bool {
	switch s {
	case "debug", "info", "warn", "error":
		return true
	}

	return false
}

// recorderEnvNames maps RecorderConfig field names to their variable names.
func recorderEnvNames() map[string]string {
	t := reflect.TypeFor[RecorderConfig]()
	m := make(map[string]string, t.NumField())
	for f := range t.Fields() {
		m[f.Name] = f.Tag.Get("env")
	}

	return m
}
