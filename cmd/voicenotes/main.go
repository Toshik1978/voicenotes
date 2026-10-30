// Command voicenotes is the VoiceNotes service entry point.
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

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: voicenotes <version|run> (run is not implemented yet)")
		return 2
	}

	switch args[0] {
	case "version":
		_, _ = fmt.Fprintf(stdout, "VoiceNotes %s (commit %s, built %s)\n", Version, Commit, Buildstamp)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\nusage: voicenotes <version|run>\n", args[0])
		return 2
	}
}
