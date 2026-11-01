package capture

import (
	"testing"
	"time"
)

func TestDoclingTimeout(t *testing.T) {
	tests := []struct {
		name         string
		before       int
		wantTimeouts int
		wantClass    Class
		wantState    FileState
		wantFailure  string
		wantWaiting  WaitingOn
	}{
		{"first timeout is temporary", 0, 1, ClassTemporary, FilePending, "", WaitingDocling},
		{"second timeout is temporary", 1, 2, ClassTemporary, FilePending, "", WaitingDocling},
		{"third timeout is permanent", 2, 3, ClassPermanent, FileFailed, "couldn't read in time", ""},
		{"later timeouts stay permanent", 3, 4, ClassPermanent, FileFailed, "couldn't read in time", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &File{Timeouts: tt.before, State: FilePending}
			got := DoclingTimeout(f)
			if got.Class != tt.wantClass {
				t.Errorf("class = %v, want %v", got.Class, tt.wantClass)
			}
			if got.WaitingOn != tt.wantWaiting {
				t.Errorf("waiting = %q, want %q", got.WaitingOn, tt.wantWaiting)
			}
			if f.Timeouts != tt.wantTimeouts {
				t.Errorf("timeouts = %d, want %d", f.Timeouts, tt.wantTimeouts)
			}
			if f.State != tt.wantState {
				t.Errorf("state = %q, want %q", f.State, tt.wantState)
			}
			if f.Failure != tt.wantFailure {
				t.Errorf("failure = %q, want %q", f.Failure, tt.wantFailure)
			}
		})
	}
}

func TestNeedsVision(t *testing.T) {
	long := "a receipt with plenty of readable words"
	tests := []struct {
		name       string
		f          File
		enabled    bool
		docling    bool
		wantVision bool
	}{
		{"disabled", File{Kind: FileImage}, false, false, false},
		{"photo, docling not configured", File{Kind: FileImage}, true, false, true},
		{"photo, docling not configured, text ignored", File{Kind: FileImage, Text: long}, true, false, true},
		{"photo, docling gave no text", File{Kind: FileImage}, true, true, true},
		{"photo, docling gave 19 runes", File{Kind: FileImage, Text: "abcdefghij klmnopqrs"}, true, true, true},
		{
			"photo, spaces do not count",
			File{Kind: FileImage, Text: " a b\n c\t d e f g h i j k l m n o p q r s "},
			true,
			true,
			true,
		},
		{"photo, docling gave 20 runes", File{Kind: FileImage, Text: "abcdefghij klmnopqrst"}, true, true, false},
		{"photo, 20 multibyte runes", File{Kind: FileImage, Text: "ёёёёёёёёёёёёёёёёёёёё"}, true, true, false},
		{"photo, docling gave plenty", File{Kind: FileImage, Text: long}, true, true, false},
		{"document never", File{Kind: FileDocument}, true, false, false},
		{"audio never", File{Kind: FileAudio}, true, false, false},
		{"video never", File{Kind: FileVideo}, true, true, false},
		{
			"already read by vision",
			File{Kind: FileImage, Text: long, TextSource: TextVision, State: FileDone},
			true,
			false,
			false,
		},
		{"budget spent", File{Kind: FileImage, State: FileEmpty, VisionAttempts: 3}, true, false, false},
		{"empty without vision tries", File{Kind: FileImage, State: FileEmpty}, true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeedsVision(tt.f, tt.enabled, tt.docling); got != tt.wantVision {
				t.Errorf("NeedsVision = %v, want %v", got, tt.wantVision)
			}
		})
	}
}

func TestVisionFailed(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name         string
		attempts     int
		started      time.Time
		out          Outcome
		now          time.Time
		wantAttempts int
		wantStarted  time.Time
		wantClass    Class
		wantState    FileState
	}{
		{"first temporary retries", 0, time.Time{}, Temporary("http 503"), t0, 1, t0, ClassTemporary, FilePending},
		{
			"second misconfig retries",
			1,
			t0,
			Misconfig("http 401"),
			t0.Add(time.Second),
			2,
			t0,
			ClassMisconfig,
			FilePending,
		},
		{"third attempt exhausts", 2, t0, Temporary("http 503"), t0.Add(time.Second), 3, t0, ClassOK, FileEmpty},
		{"two minutes exhaust", 1, t0, Temporary("timeout"), t0.Add(2 * time.Minute), 2, t0, ClassOK, FileEmpty},
		{
			"just under two minutes retries",
			1,
			t0,
			Temporary("timeout"),
			t0.Add(2*time.Minute - time.Nanosecond),
			2,
			t0,
			ClassTemporary,
			FilePending,
		},
		{"permanent is empty at once", 0, time.Time{}, Permanent("http 400"), t0, 1, t0, ClassOK, FileEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &File{Kind: FileImage, State: FilePending, VisionAttempts: tt.attempts, VisionStartedAt: tt.started}
			got := VisionFailed(f, tt.out, tt.now)
			if got.Class != tt.wantClass {
				t.Errorf("class = %v, want %v", got.Class, tt.wantClass)
			}
			if got.WaitingOn != "" {
				t.Errorf("waiting = %q, want none", got.WaitingOn)
			}
			if tt.wantClass == ClassTemporary || tt.wantClass == ClassMisconfig {
				if got.Reason != tt.out.Reason {
					t.Errorf("reason = %q, want %q", got.Reason, tt.out.Reason)
				}
			}
			if f.VisionAttempts != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d", f.VisionAttempts, tt.wantAttempts)
			}
			if !f.VisionStartedAt.Equal(tt.wantStarted) {
				t.Errorf("started = %v, want %v", f.VisionStartedAt, tt.wantStarted)
			}
			if f.State != tt.wantState {
				t.Errorf("state = %q, want %q", f.State, tt.wantState)
			}
			if f.Failure != "" {
				t.Errorf("failure = %q, want none (empty, not failed)", f.Failure)
			}
		})
	}
}
