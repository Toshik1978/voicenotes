// Command voicenotes-mic is the VoiceNotes recorder: it records the USB
// microphone kit on a key tap and hands each recording to the service. It is
// the only binary that uses cgo (constitution IV).
package main

import (
	"fmt"
	"io"
	"os"
)

// Set via -ldflags "-X main.Buildstamp=... -X main.Commit=...".
//
//nolint:gochecknoglobals // the linker's -X flag can only set package-level variables.
var (
	Buildstamp = "unknown"
	Commit     = "unknown"
	Version    = "dev"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = "usage: voicenotes-mic [run|health|version]"

func run(args []string, stdout, stderr io.Writer) int {
	return dispatch(args, stdout, stderr, func() int {
		ctx, stop := notifyContext()
		defer stop()

		return runRecorder(ctx, os.Environ(), stderr)
	})
}

// dispatch routes the command line; no command (or "run") starts the recorder.
func dispatch(args []string, stdout, stderr io.Writer, recorder func() int) int {
	if len(args) == 0 {
		return recorder()
	}

	switch args[0] {
	case "version":
		_, _ = fmt.Fprintf(stdout, "voicenotes-mic %s (commit %s, built %s)\n", Version, Commit, Buildstamp)
		return 0
	case "run":
		return recorder()
	case "health":
		return runHealth(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}
