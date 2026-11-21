package micdev

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Toshik1978/voicenotes/internal/mic"
)

// Sysfs is a view of /sys with symlink resolution.
//
// An fs.FS cannot follow symlinks to learn where an entry really lives, so
// the two concerns are separate: FS reads files and lists directories (paths
// relative to the root, slash separated), and Real maps such a path to the
// real device path it resolves to, again relative to the root. Production
// uses [HostSysfs]; tests use a plain directory tree and a map-backed Real.
type Sysfs struct {
	FS   fs.FS
	Real func(rel string) (string, error)
}

// HostSysfs returns the view of the real /sys.
func HostSysfs() Sysfs { return hostSysfs("/sys") }

func hostSysfs(root string) Sysfs {
	return Sysfs{
		FS: os.DirFS(root),
		Real: func(rel string) (string, error) {
			resolved, err := filepath.EvalSymlinks(filepath.Join(root, rel))
			if err != nil {
				return "", fmt.Errorf("micdev: resolve %s: %w", rel, err)
			}

			out, err := filepath.Rel(root, resolved)
			if err != nil {
				return "", fmt.Errorf("micdev: relate %s: %w", resolved, err)
			}

			return filepath.ToSlash(out), nil
		},
	}
}

// Discover walks the sound cards and input devices and matches a receiver
// (R4). It implements [recorder.Discovery].
type Discover struct {
	Sys Sysfs
}

// Discover reports the receiver, or false when none is connected.
func (d Discover) Discover() (mic.Receiver, bool, error) {
	snap, err := d.snapshot()
	if err != nil {
		return mic.Receiver{}, false, err
	}

	rcv, ok := mic.MatchReceiver(snap)

	return rcv, ok, nil
}

var (
	cardRe  = regexp.MustCompile(`^card(\d+)$`)
	eventRe = regexp.MustCompile(`^event\d+$`)
	pcmRe   = regexp.MustCompile(`^pcmC\d+D\d+c$`)
	// usbDevRe names a USB device directory (1-1, 1-1.2), not an interface
	// (1-1.2:1.0) and not a root hub (usb1).
	usbDevRe = regexp.MustCompile(`^\d+-\d+(\.\d+)*$`)
)

func (d Discover) snapshot() (mic.Snapshot, error) {
	var snap mic.Snapshot

	cards, err := d.cards()
	if err != nil {
		return snap, err
	}

	inputs, err := d.inputs()
	if err != nil {
		return snap, err
	}

	snap.Cards, snap.Inputs = cards, inputs

	return snap, nil
}

func (d Discover) cards() ([]mic.SoundCard, error) {
	entries, err := readDir(d.Sys.FS, "class/sound")
	if err != nil {
		return nil, err
	}

	var out []mic.SoundCard

	for _, name := range entries {
		m := cardRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}

		n, _ := strconv.Atoi(m[1])
		dir := path.Join("class/sound", name)

		children, err := readDir(d.Sys.FS, dir)
		if err != nil {
			return nil, err
		}

		card := mic.SoundCard{Number: n}

		for _, c := range children {
			if pcmRe.MatchString(c) {
				card.HasCapture = true
			}
		}

		card.USBParent = d.usbParent(dir)
		out = append(out, card)
	}

	return out, nil
}

func (d Discover) inputs() ([]mic.InputDevice, error) {
	entries, err := readDir(d.Sys.FS, "class/input")
	if err != nil {
		return nil, err
	}

	var out []mic.InputDevice

	for _, name := range entries {
		if !eventRe.MatchString(name) {
			continue
		}

		dir := path.Join("class/input", name)

		key, err := fs.ReadFile(d.Sys.FS, path.Join(dir, "device/capabilities/key"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("micdev: read key capabilities of %s: %w", name, err)
		}

		out = append(out, mic.InputDevice{
			Path:      "/dev/input/" + name,
			KeyBitmap: strings.TrimSpace(string(key)),
			USBParent: d.usbParent(dir),
		})
	}

	return out, nil
}

// usbParent resolves rel to its real path and returns the nearest ancestor
// that is a USB device directory, or "" if there is none (or the entry
// vanished, as during an unplug).
func (d Discover) usbParent(rel string) string {
	resolved, err := d.Sys.Real(rel)
	if err != nil {
		return ""
	}

	return usbAncestor(resolved)
}

// usbAncestor returns the prefix of p ending at its deepest component that
// looks like a USB device (1-1.2), ignoring interfaces (1-1.2:1.0).
func usbAncestor(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range slices.Backward(parts) {
		if usbDevRe.MatchString(part) {
			return strings.Join(parts[:i+1], "/")
		}
	}

	return ""
}

// readDir lists the entry names of dir; a missing directory is empty (no
// sound subsystem, no input devices).
func readDir(fsys fs.FS, dir string) ([]string, error) {
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("micdev: list %s: %w", dir, err)
	}

	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}

	return names, nil
}

// Fixed is the explicit device override (MIC_CAPTURE_DEVICE together with
// MIC_INPUT_DEVICE): it always reports the configured pair, without sysfs. It
// implements [recorder.Discovery]. The receiver carries the capture device
// name in CaptureDevice; Card is unused.
type Fixed struct {
	CaptureDevice string
	InputPath     string
}

// Discover returns the configured pair.
func (f Fixed) Discover() (mic.Receiver, bool, error) {
	return mic.Receiver{InputPath: f.InputPath, CaptureDevice: f.CaptureDevice}, true, nil
}
