// Package micdev is the recorder's Linux device adapter: the receiver's key
// input (evdev) and its discovery through sysfs. It uses only the standard
// library and package syscall, so it stays CGO-free.
package micdev

import (
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/recorder"
)

// eviocgrab is the EVIOCGRAB ioctl request: _IOW('E', 0x90, int).
const eviocgrab = 0x40044590

// Keys opens evdev devices; it implements [recorder.KeySource].
type Keys struct{}

var _ recorder.KeySource = Keys{}

// Open opens the input device at path read-only and tries to grab it, so the
// key neither changes the host volume nor reaches other programs. A failed
// grab is not an error: the stream is returned with Grabbed false and a
// Regrab the recorder retries on a schedule.
func (Keys) Open(path string) (recorder.KeyStream, error) {
	// The path is operator configuration or discovered under /dev/input.
	f, err := os.Open(path) //nolint:gosec // G304,G703: see above
	if err != nil {
		return recorder.KeyStream{}, fmt.Errorf("micdev: open input device: %w", err)
	}

	grabErr := grab(f)

	st := newStream(f, f, grabErr == nil)
	st.GrabErr = grabErr
	st.Regrab = func() error { return grab(f) }

	return st, nil
}

// grab takes exclusive access to the device. It goes through SyscallConn so
// the file stays in non-blocking mode and Close can interrupt a read.
func grab(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("micdev: raw conn: %w", err)
	}

	var errno syscall.Errno

	err = rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, eviocgrab, 1)
	})
	if err != nil {
		return fmt.Errorf("micdev: control: %w", err)
	}

	if errno != 0 {
		return fmt.Errorf("micdev: grab input device: %w", errno)
	}

	return nil
}

// newStream starts the decode loop over r and returns the stream. Closing the
// stream closes c, which must make a blocked read on r return.
func newStream(r io.Reader, c io.Closer, grabbed bool) recorder.KeyStream {
	events := make(chan recorder.KeyEvent, 8)
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		decode(r, events, done)
	}()

	var once sync.Once

	var closeErr error

	return recorder.KeyStream{
		Events:  events,
		Grabbed: grabbed,
		Close: func() error {
			once.Do(func() {
				close(done)

				if err := c.Close(); err != nil {
					closeErr = fmt.Errorf("micdev: close input device: %w", err)
				}

				<-finished
			})

			return closeErr
		},
	}
}

// decode reads 24-byte input_event records from r and sends a KeyTap for each
// press. When r fails (ENODEV after an unplug, EOF, any read error) it sends
// KeyLost, unless done is closed. It always closes events on return.
func decode(r io.Reader, events chan<- recorder.KeyEvent, done <-chan struct{}) {
	defer close(events)

	var buf [mic.EventSize]byte

	for {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			// ENODEV, EOF or any other error: the device is gone for our purposes.
			send(events, done, recorder.KeyLost)

			return
		}

		if mic.IsTap(mic.DecodeEvent(buf)) && !send(events, done, recorder.KeyTap) {
			return
		}
	}
}

// send delivers ev unless done closes first, and reports whether it did.
func send(events chan<- recorder.KeyEvent, done <-chan struct{}, ev recorder.KeyEvent) bool {
	select {
	case <-done:
		return false
	default:
	}

	select {
	case events <- ev:
		return true
	case <-done:
		return false
	}
}
