package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const fakeToken = "FAKE-0123456789abcdef0123456789abcdef"

func recEnv(kv ...string) []string {
	e := []string{
		"MIC_SERVER_URL=http://voicenotes.example.org:8080",
		"MIC_UPLOAD_TOKEN=" + fakeToken,
	}
	for i := 0; i < len(kv); i += 2 {
		out := e[:0:0]
		for _, kvp := range e {
			if !strings.HasPrefix(kvp, kv[i]+"=") {
				out = append(out, kvp)
			}
		}
		e = out
		if kv[i+1] != "-" {
			e = append(e, kv[i]+"="+kv[i+1])
		}
	}

	return e
}

// parseRec parses with a fixed hostname lookup.
func parseRec(environ []string, name string, err error) (RecorderConfig, error) {
	return parseRecorder(environ, func() (string, error) { return name, err })
}

func pr(environ []string) (RecorderConfig, error) { return parseRec(environ, "kitchen-pi", nil) }

func TestParseRecorderDefaults(t *testing.T) {
	c, err := pr(recEnv())
	if err != nil {
		t.Fatalf("ParseRecorder: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"ServerURL", c.ServerURL, "http://voicenotes.example.org:8080"},
		{"UploadToken", c.UploadToken, fakeToken},
		{"RecorderName", c.RecorderName, "kitchen-pi"},
		{"CaptureDevice", c.CaptureDevice, ""},
		{"InputDevice", c.InputDevice, ""},
		{"Debounce", c.Debounce, 500 * time.Millisecond},
		{"MinLength", c.MinLength, 2 * time.Second},
		{"MaxLength", c.MaxLength, 10 * time.Minute},
		{"SpoolDir", c.SpoolDir, "/var/lib/voicenotes-mic"},
		{"SpoolMinFree", c.SpoolMinFree, int64(268435456)},
		{"CheckinInterval", c.CheckinInterval, time.Minute},
		{"RetryMaxInterval", c.RetryMaxInterval, 5 * time.Minute},
		{"PendingThreshold", c.PendingThreshold, 30 * time.Minute},
		{"HTTPAddr", c.HTTPAddr, "127.0.0.1:8081"},
		{"LogLevel", c.LogLevel, "info"},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %v, want %v", ck.name, ck.got, ck.want)
		}
	}
}

func TestParseRecorderOverrides(t *testing.T) {
	c, err := pr(recEnv(
		"MIC_RECORDER_NAME", "desk.1_a-b",
		"MIC_CAPTURE_DEVICE", "Wireless Mic",
		"MIC_INPUT_DEVICE", "/dev/input/event7",
		"MIC_DEBOUNCE", "0s",
		"MIC_MIN_LENGTH", "60s",
		"MIC_MAX_LENGTH", "12m",
		"MIC_SPOOL_DIR", "/srv/spool",
		"STATE_DIRECTORY", "/var/lib/other",
		"MIC_SPOOL_MIN_FREE", "999999999",
		"LOG_LEVEL", "debug",
	))
	if err != nil {
		t.Fatalf("ParseRecorder: %v", err)
	}
	if c.RecorderName != "desk.1_a-b" || c.CaptureDevice != "Wireless Mic" ||
		c.InputDevice != "/dev/input/event7" || c.SpoolDir != "/srv/spool" ||
		c.MaxLength != 12*time.Minute || c.LogLevel != "debug" {
		t.Errorf("unexpected config: %+v", c)
	}
}

func TestParseRecorderStateDirectory(t *testing.T) {
	c, err := pr(recEnv("STATE_DIRECTORY", "/var/lib/voicenotes-state"))
	if err != nil {
		t.Fatalf("ParseRecorder: %v", err)
	}
	if c.SpoolDir != "/var/lib/voicenotes-state" {
		t.Errorf("SpoolDir = %q", c.SpoolDir)
	}
}

func TestParseRecorderErrors(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"missing url", recEnv("MIC_SERVER_URL", "-"), "MIC_SERVER_URL: is required"},
		{"relative url", recEnv("MIC_SERVER_URL", "voicenotes.example.org"), "MIC_SERVER_URL: must be an http"},
		{"ftp url", recEnv("MIC_SERVER_URL", "ftp://example.org"), "MIC_SERVER_URL: must be an http"},
		{"missing token", recEnv("MIC_UPLOAD_TOKEN", "-"), "MIC_UPLOAD_TOKEN: is required"},
		{"short token", recEnv("MIC_UPLOAD_TOKEN", "short-secret"), "MIC_UPLOAD_TOKEN: must be at least 32"},
		{"bad name", recEnv("MIC_RECORDER_NAME", "bad name!"), "MIC_RECORDER_NAME"},
		{"long name", recEnv("MIC_RECORDER_NAME", strings.Repeat("a", 65)), "MIC_RECORDER_NAME"},
		{"capture only", recEnv("MIC_CAPTURE_DEVICE", "Mic"), "MIC_INPUT_DEVICE: is required when MIC_CAPTURE_DEVICE"},
		{
			"input only",
			recEnv("MIC_INPUT_DEVICE", "/dev/input/event1"),
			"MIC_CAPTURE_DEVICE: is required when MIC_INPUT_DEVICE",
		},
		{"debounce high", recEnv("MIC_DEBOUNCE", "6s"), "MIC_DEBOUNCE: must be between"},
		{"debounce negative", recEnv("MIC_DEBOUNCE", "-1s"), "MIC_DEBOUNCE: must be between"},
		{"debounce junk", recEnv("MIC_DEBOUNCE", "soon"), "MIC_DEBOUNCE: invalid value"},
		{"min high", recEnv("MIC_MIN_LENGTH", "61s"), "MIC_MIN_LENGTH: must be between"},
		{"max low", recEnv("MIC_MAX_LENGTH", "9s"), "MIC_MAX_LENGTH: must be between"},
		{"max high", recEnv("MIC_MAX_LENGTH", "13m"), "MIC_MAX_LENGTH: must be between"},
		{
			"min not below max",
			recEnv("MIC_MIN_LENGTH", "30s", "MIC_MAX_LENGTH", "30s", "MIC_SPOOL_MIN_FREE", "10000000"),
			"MIC_MIN_LENGTH: must be shorter than MIC_MAX_LENGTH",
		},
		{"free low", recEnv("MIC_SPOOL_MIN_FREE", "1000"), "MIC_SPOOL_MIN_FREE: must be at least"},
		{"free just below", recEnv("MIC_SPOOL_MIN_FREE", "38399999", "MIC_MAX_LENGTH", "10m"), "MIC_SPOOL_MIN_FREE"},
		{"free junk", recEnv("MIC_SPOOL_MIN_FREE", "lots"), "MIC_SPOOL_MIN_FREE: invalid value"},
		{"checkin low", recEnv("MIC_CHECKIN_INTERVAL", "9s"), "MIC_CHECKIN_INTERVAL: must be between"},
		{"checkin high", recEnv("MIC_CHECKIN_INTERVAL", "11m"), "MIC_CHECKIN_INTERVAL: must be between"},
		{"retry low", recEnv("MIC_RETRY_MAX_INTERVAL", "9s"), "MIC_RETRY_MAX_INTERVAL: must be between"},
		{"retry high", recEnv("MIC_RETRY_MAX_INTERVAL", "61m"), "MIC_RETRY_MAX_INTERVAL: must be between"},
		{"pending zero", recEnv("MIC_PENDING_THRESHOLD", "0s"), "MIC_PENDING_THRESHOLD: must be positive"},
		{"empty addr", recEnv("MIC_HTTP_ADDR", " "), "MIC_HTTP_ADDR"},
		{"log level", recEnv("LOG_LEVEL", "loud"), "LOG_LEVEL: must be debug"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pr(tc.env)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestParseRecorderSpoolFloorBoundary(t *testing.T) {
	// 2 * 10m * 32000 B/s = 38400000.
	if _, err := pr(recEnv("MIC_SPOOL_MIN_FREE", "38400000")); err != nil {
		t.Errorf("exact floor rejected: %v", err)
	}
	// 2 * 12m * 32000 = 46080000 raises the floor with MIC_MAX_LENGTH.
	if _, err := pr(recEnv("MIC_MAX_LENGTH", "12m", "MIC_SPOOL_MIN_FREE", "40000000")); err == nil {
		t.Error("floor must follow MIC_MAX_LENGTH")
	}
}

func TestParseRecorderHostnameFailure(t *testing.T) {
	broken := errors.New("no hostname")
	if _, err := parseRec(recEnv(), "", broken); err == nil || !strings.Contains(err.Error(), "MIC_RECORDER_NAME") {
		t.Errorf("err = %v", err)
	}
	// An explicit name makes the hostname irrelevant.
	if _, err := parseRec(recEnv("MIC_RECORDER_NAME", "x"), "", broken); err != nil {
		t.Errorf("explicit name: %v", err)
	}
}

func TestParseRecorderIgnoresServiceVariables(t *testing.T) {
	// Service variables, valid or not, neither required nor consulted.
	if _, err := pr(recEnv("TELEGRAM_BOT_TOKEN", "x", "STT_TIMEOUT", "junk", "TZ", "Nowhere/Land")); err != nil {
		t.Errorf("service variables must be ignored: %v", err)
	}
}

func TestParseRecorderNeverPrintsValues(t *testing.T) {
	_, err := pr(recEnv("MIC_UPLOAD_TOKEN", secretValue, "MIC_SERVER_URL", "ftp://"+secretValue))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("error leaks a value: %v", err)
	}
}

func TestParseRecorderReportsAllProblems(t *testing.T) {
	_, err := pr(recEnv("MIC_SERVER_URL", "-", "MIC_UPLOAD_TOKEN", "-", "LOG_LEVEL", "loud"))
	if err == nil {
		t.Fatal("want error")
	}
	for _, w := range []string{"MIC_SERVER_URL", "MIC_UPLOAD_TOKEN", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("missing %s in %v", w, err)
		}
	}
}

func TestParseRecorderPublicEntryPoint(t *testing.T) {
	c, err := ParseRecorder(recEnv("MIC_RECORDER_NAME", "desk"))
	if err != nil {
		t.Fatalf("ParseRecorder: %v", err)
	}
	if c.RecorderName != "desk" {
		t.Errorf("RecorderName = %q", c.RecorderName)
	}
}
