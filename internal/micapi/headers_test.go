package micapi

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	testToken = "FAKE-0123456789abcdef0123456789abcdef"
	testHash  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func TestAuthorized(t *testing.T) {
	tests := []struct {
		name, header, token string
		want                bool
	}{
		{"match", "Bearer " + testToken, testToken, true},
		{"scheme case-insensitive", "bearer " + testToken, testToken, true},
		{"missing", "", testToken, false},
		{"wrong token", "Bearer FAKE-wrong", testToken, false},
		{"prefix of token", "Bearer " + testToken[:10], testToken, false},
		{"other scheme", "Basic " + testToken, testToken, false},
		{"no space", "Bearer" + testToken, testToken, false},
		{"empty configured token", "Bearer ", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorized(tt.header, tt.token); got != tt.want {
				t.Fatalf("authorized(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestParseRecordingID(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"9223372036854775807", 9223372036854775807, true},
		{"0", 0, false},
		{"-5", 0, false},
		{"+5", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"9223372036854775808", 0, false},
		{" 5", 0, false},
		{"0x10", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, rej := parseRecordingID(tt.in)
			if tt.ok != (rej == nil) || got != tt.want {
				t.Fatalf("parseRecordingID(%q) = %d, %v", tt.in, got, rej)
			}
			if rej != nil && (rej.code != http.StatusBadRequest || rej.status != statusBadRequest) {
				t.Fatalf("rejection = %+v", rej)
			}
		})
	}
}

func TestValidRecorderName(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{"bedside", true},
		{"Pi-4.kitchen_2", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"bad name", false},
		{"bad/name", false},
		{"имя", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := validRecorderName(tt.in); got != tt.ok {
				t.Fatalf("validRecorderName(%q) = %v", tt.in, got)
			}
		})
	}
}

func uploadHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "audio/wav")
	h.Set("X-Content-Sha256", testHash)
	h.Set("X-Recording-Started-At", "2026-11-20T06:30:01.123456789Z")
	h.Set("X-Recording-End-Reason", "tap")
	h.Set("X-Recorder", "bedside")

	return h
}

func TestParseUploadOK(t *testing.T) {
	up, rej := parseUpload(uploadHeaders(), 1000, 2000)
	if rej != nil {
		t.Fatal(rej)
	}
	want := time.Date(2026, 11, 20, 6, 30, 1, 123456789, time.UTC)
	if !up.startedAt.Equal(want) || up.startedAt.Location() != time.UTC {
		t.Fatalf("startedAt = %v", up.startedAt)
	}
	if up.length != 1000 || up.endReason != "tap" || up.recorder != "bedside" || up.sha256 != testHash {
		t.Fatalf("upload = %+v", up)
	}
}

func TestParseUploadOffsetTimeBecomesUTC(t *testing.T) {
	h := uploadHeaders()
	h.Set("X-Recording-Started-At", "2026-11-20T08:30:01+02:00")
	up, rej := parseUpload(h, 10, 20)
	if rej != nil {
		t.Fatal(rej)
	}
	if up.startedAt != time.Date(2026, 11, 20, 6, 30, 1, 0, time.UTC) {
		t.Fatalf("startedAt = %v", up.startedAt)
	}
}

func TestParseUploadContentTypeParams(t *testing.T) {
	h := uploadHeaders()
	h.Set("Content-Type", "Audio/WAV; charset=binary")
	if _, rej := parseUpload(h, 10, 20); rej != nil {
		t.Fatal(rej)
	}
}

func TestParseUploadRejections(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(http.Header)
		length int64
		code   int
	}{
		{"wrong content type", func(h http.Header) { h.Set("Content-Type", "audio/ogg") }, 10, 415},
		{"missing content type", func(h http.Header) { h.Del("Content-Type") }, 10, 415},
		{"too large", func(http.Header) {}, 2001, 413},
		{"unknown length", func(http.Header) {}, -1, 400},
		{"missing hash", func(h http.Header) { h.Del("X-Content-Sha256") }, 10, 400},
		{"uppercase hash", func(h http.Header) { h.Set("X-Content-Sha256", strings.ToUpper(testHash)) }, 10, 400},
		{"short hash", func(h http.Header) { h.Set("X-Content-Sha256", testHash[:10]) }, 10, 400},
		{"missing started", func(h http.Header) { h.Del("X-Recording-Started-At") }, 10, 400},
		{"bad started", func(h http.Header) { h.Set("X-Recording-Started-At", "yesterday") }, 10, 400},
		{"missing end reason", func(h http.Header) { h.Del("X-Recording-End-Reason") }, 10, 400},
		{"bad end reason", func(h http.Header) { h.Set("X-Recording-End-Reason", "bored") }, 10, 400},
		{"missing recorder", func(h http.Header) { h.Del("X-Recorder") }, 10, 400},
		{"bad recorder", func(h http.Header) { h.Set("X-Recorder", "a b") }, 10, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := uploadHeaders()
			tt.edit(h)
			_, rej := parseUpload(h, tt.length, 2000)
			if rej == nil || rej.code != tt.code {
				t.Fatalf("rejection = %+v, want code %d", rej, tt.code)
			}
		})
	}
}

func TestParseEndReasons(t *testing.T) {
	for _, r := range []string{"tap", "max_length", "receiver_lost", "interrupted"} {
		h := uploadHeaders()
		h.Set("X-Recording-End-Reason", r)
		if _, rej := parseUpload(h, 10, 20); rej != nil {
			t.Fatalf("%s: %v", r, rej)
		}
	}
}

func TestParseStarted(t *testing.T) {
	at, name, rej := parseStarted([]byte(`{"started_at":"2026-11-20T06:30:01.5Z","recorder":"bedside"}`))
	if rej != nil {
		t.Fatal(rej)
	}
	if name != "bedside" || at != time.Date(2026, 11, 20, 6, 30, 1, 500000000, time.UTC) {
		t.Fatalf("got %v %q", at, name)
	}

	for _, body := range []string{
		``,
		`not json`,
		`{"recorder":"bedside"}`,
		`{"started_at":"soon","recorder":"bedside"}`,
		`{"started_at":"2026-11-20T06:30:01Z"}`,
		`{"started_at":"2026-11-20T06:30:01Z","recorder":"a b"}`,
		`{"started_at":"2026-11-20T06:30:01Z","recorder":"bedside"} trailing`,
	} {
		if _, _, rej := parseStarted([]byte(body)); rej == nil || rej.code != http.StatusBadRequest {
			t.Fatalf("parseStarted(%q) rejection = %+v", body, rej)
		}
	}
}

func TestParseCheckIn(t *testing.T) {
	c, rej := parseCheckIn([]byte(`{"receiver_present":true,"storage_full":false,"pending":3,` +
		`"oldest_pending_at":"2026-11-20T06:30:01Z"}`))
	if rej != nil {
		t.Fatal(rej)
	}
	if !c.receiverPresent || c.storageFull {
		t.Fatalf("check-in = %+v", c)
	}

	_, rej = parseCheckIn([]byte(`{"receiver_present":false,"storage_full":true,"pending":0,"oldest_pending_at":null}`))
	if rej != nil {
		t.Fatal(rej)
	}

	for _, body := range []string{
		``,
		`[]`,
		`{"storage_full":false}`,
		`{"receiver_present":true}`,
		`{"receiver_present":true,"storage_full":false,"pending":-1}`,
		`{"receiver_present":"yes","storage_full":false}`,
		`{"receiver_present":true,"storage_full":false,"oldest_pending_at":"then"}`,
	} {
		if _, rej := parseCheckIn([]byte(body)); rej == nil || rej.code != http.StatusBadRequest {
			t.Fatalf("parseCheckIn(%q) rejection = %+v", body, rej)
		}
	}
}

func TestRejectionError(t *testing.T) {
	if got := badRequest("nope").Error(); got != "400 bad_request: nope" {
		t.Fatalf("Error() = %q", got)
	}
}
