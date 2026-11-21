package micaudio

import (
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gen2brain/malgo"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/recorder"
)

var _ recorder.AudioSource = (*Capture)(nil)

func TestCaptureConfig(t *testing.T) {
	cfg := captureConfig()
	if cfg.DeviceType != malgo.Capture {
		t.Errorf("DeviceType = %v, want Capture", cfg.DeviceType)
	}
	if cfg.Capture.Format != malgo.FormatS16 {
		t.Errorf("Format = %v, want FormatS16", cfg.Capture.Format)
	}
	if cfg.Capture.Channels != mic.Channels {
		t.Errorf("Channels = %d, want %d", cfg.Capture.Channels, mic.Channels)
	}
	if cfg.SampleRate != mic.SampleRate {
		t.Errorf("SampleRate = %d, want %d", cfg.SampleRate, mic.SampleRate)
	}
}

func TestSelectDevice(t *testing.T) {
	devs := []candidate{
		{ID: "default", Name: "Default Audio Device"},
		{ID: "hw:0,0", Name: "HDA Intel PCH"},
		{ID: "hw:1,0", Name: "USB Receiver 2.4G"},
		{ID: "hw:10,0", Name: "Other Gadget"},
	}
	tests := []struct {
		name string
		rcv  mic.Receiver
		want int
		ok   bool
	}{
		{"card 1", mic.Receiver{Card: 1}, 2, true},
		{"card 0", mic.Receiver{Card: 0}, 1, true},
		{"card 10 is not card 1", mic.Receiver{Card: 10}, 3, true},
		{"card missing", mic.Receiver{Card: 7}, 0, false},
		{"name overrides card", mic.Receiver{Card: 0, CaptureDevice: "Gadget"}, 3, true},
		{"id substring", mic.Receiver{CaptureDevice: "hw:1,0"}, 2, true},
		{"name missing", mic.Receiver{Card: 1, CaptureDevice: "Nope"}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := selectDevice(devs, tt.rcv)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Errorf("selectDevice = %d, %v; want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCardOfID(t *testing.T) {
	tests := []struct {
		id   string
		card int
		ok   bool
	}{
		{"hw:3,0", 3, true},
		{"plughw:2,1", 2, true},
		{"hw:4", 4, true},
		{"hw:CARD=Foo,DEV=0", 0, false},
		{"default", 0, false},
		{"hw:,0", 0, false},
	}
	for _, tt := range tests {
		card, ok := cardOfID(tt.id)
		if ok != tt.ok || card != tt.card {
			t.Errorf("cardOfID(%q) = %d, %v; want %d, %v", tt.id, card, ok, tt.card, tt.ok)
		}
	}
}

func TestDataProcCopiesAndNeverBlocks(t *testing.T) {
	q := recorder.NewFrameQueue(1)
	proc := dataProc(q)

	buf := []byte{1, 2, 3, 4}
	proc(nil, buf, 2)
	buf[0] = 99 // miniaudio reuses its buffer

	done := make(chan struct{})
	go func() {
		proc(nil, []byte{5, 6}, 1) // queue full: must drop, not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("data callback blocked on a full queue")
	}
	if q.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1", q.Dropped())
	}
	proc(nil, nil, 0) // empty input is ignored
}

func TestStopProcSignalsOnce(t *testing.T) {
	lost := make(chan error, 1)
	closed := &atomic.Bool{}
	stop := stopProc(lost, closed)

	stop()
	stop() // a second stop must not block
	select {
	case err := <-lost:
		if !errors.Is(err, recorder.ErrDeviceLost) {
			t.Errorf("err = %v, want ErrDeviceLost", err)
		}
	default:
		t.Fatal("no device-lost signal")
	}
}

func TestStopProcSilentAfterClose(t *testing.T) {
	lost := make(chan error, 1)
	closed := &atomic.Bool{}
	closed.Store(true)
	stopProc(lost, closed)()
	select {
	case err := <-lost:
		t.Errorf("unexpected signal after Close: %v", err)
	default:
	}
}

// TestRecordOneSecond records from the configured device; it needs real
// hardware, so it runs only with VN_MICDEV=1 (VN_MICCARD=n or VN_MICNAME=substring selects it).
func TestRecordOneSecond(t *testing.T) {
	if os.Getenv("VN_MICDEV") != "1" {
		t.Skip("set VN_MICDEV=1 to record from a real capture device")
	}
	card, _ := strconv.Atoi(os.Getenv("VN_MICCARD"))
	rcv := mic.Receiver{Card: card, CaptureDevice: os.Getenv("VN_MICNAME")}
	q := recorder.NewFrameQueue(1024)
	s, err := New().Open(rcv, q)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	time.Sleep(time.Second)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if q.Dropped() != 0 {
		t.Errorf("dropped %d frames", q.Dropped())
	}
}
