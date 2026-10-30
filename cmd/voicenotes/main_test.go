package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	oldV, oldC, oldB := Version, Commit, Buildstamp
	t.Cleanup(func() { Version, Commit, Buildstamp = oldV, oldC, oldB })
	Version, Commit, Buildstamp = "1.2.3", "abc1234", "2026-01-02T03:04:05Z"

	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"VoiceNotes", "1.2.3", "abc1234", "2026-01-02T03:04:05Z"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q missing %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunNoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)

	if code == 0 {
		t.Error("exit code = 0, want non-zero")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "usage") {
		t.Errorf("stderr %q should contain usage", stderr.String())
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"bogus"}, &stdout, &stderr); code == 0 {
		t.Error("exit code = 0, want non-zero")
	}
}
