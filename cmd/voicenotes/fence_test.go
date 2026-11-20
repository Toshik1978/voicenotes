package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestServiceHasNoCgoDependencies fails when the service's dependency graph
// reaches the recorder's audio-capture packages.
func TestServiceHasNoCgoDependencies(t *testing.T) {
	// Forbidden are the cgo-bound packages the service must never link
	// (constitution IV): only voicenotes-mic and internal/micaudio may use them.
	forbidden := []string{
		"github.com/gen2brain/malgo",
		"github.com/Toshik1978/voicenotes/internal/micaudio",
	}

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}

	out, err := exec.CommandContext(t.Context(), goBin, "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}

	deps := make(map[string]bool)
	for line := range strings.SplitSeq(string(out), "\n") {
		deps[strings.TrimSpace(line)] = true
	}

	for _, path := range forbidden {
		if deps[path] {
			t.Errorf("cmd/voicenotes depends on %s; the service must stay CGO-free", path)
		}
	}
}
