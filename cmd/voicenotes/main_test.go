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

func TestDispatchStartsServiceByDefault(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}} {
		var stdout, stderr bytes.Buffer
		started := false
		code := dispatch(args, &stdout, &stderr, func() int {
			started = true
			return 7
		})

		if !started {
			t.Errorf("args %v: service not started", args)
		}
		if code != 7 {
			t.Errorf("args %v: exit code = %d, want the service's 7", args, code)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Errorf("args %v: unexpected output %q %q", args, stdout.String(), stderr.String())
		}
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{"bogus"}, &stdout, &stderr, func() int {
		t.Error("service must not start for an unknown command")
		return 0
	})

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage: voicenotes [run|health|version]") {
		t.Errorf("stderr %q should contain usage", stderr.String())
	}
}
