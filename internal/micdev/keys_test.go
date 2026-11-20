package micdev

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/recorder"
)

func record(typ, code uint16, value int32) []byte {
	b := make([]byte, mic.EventSize)
	binary.LittleEndian.PutUint64(b[0:], 1700000000)
	binary.LittleEndian.PutUint16(b[16:], typ)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value))

	return b
}

func next(t *testing.T, ch <-chan recorder.KeyEvent) (recorder.KeyEvent, bool) {
	t.Helper()

	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a key event")
	}

	return 0, false
}

func TestStreamTapsThenLostOnEOF(t *testing.T) {
	pr, pw := io.Pipe()
	s := newStream(pr, pr, true)

	if !s.Grabbed {
		t.Error("Grabbed = false, want true")
	}

	go func() {
		for _, rec := range [][]byte{
			record(mic.EvKey, mic.KeyVolumeUp, 1),
			record(mic.EvKey, mic.KeyVolumeUp, 0), // release
			record(mic.EvKey, mic.KeyVolumeUp, 2), // autorepeat
			record(0, 0, 0),                       // sync
			record(mic.EvKey, 114, 1),             // other key
			record(mic.EvKey, mic.KeyVolumeUp, 1),
		} {
			_, _ = pw.Write(rec)
		}

		_ = pw.Close()
	}()

	for i := range 2 {
		if ev, ok := next(t, s.Events); !ok || ev != recorder.KeyTap {
			t.Fatalf("event %d = %v, %v; want KeyTap", i, ev, ok)
		}
	}

	if ev, ok := next(t, s.Events); !ok || ev != recorder.KeyLost {
		t.Fatalf("got %v, %v; want KeyLost", ev, ok)
	}

	if _, ok := next(t, s.Events); ok {
		t.Fatal("channel still open after KeyLost")
	}

	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestStreamPartialRecordIsLost(t *testing.T) {
	pr, pw := io.Pipe()
	s := newStream(pr, pr, false)

	go func() {
		_, _ = pw.Write(record(mic.EvKey, mic.KeyVolumeUp, 1)[:10])
		_ = pw.Close()
	}()

	if ev, ok := next(t, s.Events); !ok || ev != recorder.KeyLost {
		t.Fatalf("got %v, %v; want KeyLost", ev, ok)
	}

	_ = s.Close()
}

func TestStreamCloseStopsReaderWithoutLost(t *testing.T) {
	pr, _ := io.Pipe()
	s := newStream(pr, pr, false)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Closing again is harmless.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if ev, ok := next(t, s.Events); ok {
		t.Fatalf("got %v after Close; want a closed channel", ev)
	}
}

func TestStreamCloseWithTapPending(t *testing.T) {
	pr, pw := io.Pipe()
	s := newStream(pr, pr, false)

	// Fill the buffer and then block the sender; Close must still return.
	go func() {
		for range 32 {
			if _, err := pw.Write(record(mic.EvKey, mic.KeyVolumeUp, 1)); err != nil {
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})

	go func() {
		_ = s.Close()

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a full event channel")
	}
}

func TestOpenMissingDevice(t *testing.T) {
	if _, err := (Keys{}).Open(filepath.Join(t.TempDir(), "event0")); err == nil {
		t.Fatal("Open of a missing device succeeded")
	}
}

func TestOpenRegularFileIsNotGrabbed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "event0")
	if err := os.WriteFile(p, record(mic.EvKey, mic.KeyVolumeUp, 1), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Keys{}.Open(p)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = s.Close() }()

	if s.Grabbed {
		t.Error("a regular file cannot be grabbed")
	}

	if ev, ok := next(t, s.Events); !ok || ev != recorder.KeyTap {
		t.Fatalf("got %v, %v; want KeyTap", ev, ok)
	}

	if ev, _ := next(t, s.Events); ev != recorder.KeyLost {
		t.Fatalf("got %v; want KeyLost at EOF", ev)
	}
}

// TestRealDevice grabs and reads the device named by VN_MICDEV_INPUT (default
// /dev/input/event0) for a moment. It needs read access to the device.
func TestRealDevice(t *testing.T) {
	if os.Getenv("VN_MICDEV") != "1" {
		t.Skip("set VN_MICDEV=1 to open a real input device")
	}

	dev := os.Getenv("VN_MICDEV_INPUT")
	if dev == "" {
		dev = "/dev/input/event0"
	}

	s, err := Keys{}.Open(dev)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("grabbed: %v", s.Grabbed)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
