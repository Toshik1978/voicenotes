// Package micaudio is the recorder's cgo audio adapter: it captures s16le
// 16 kHz mono audio through miniaudio (malgo) over ALSA. Only
// cmd/voicenotes-mic may import it (constitution IV).
package micaudio

/*
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/gen2brain/malgo"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/recorder"
)

// Capture implements [recorder.AudioSource].
type Capture struct{}

// New returns the audio source.
func New() *Capture { return &Capture{} }

// candidate is a capture device as listed by the backend.
type candidate struct {
	ID   string
	Name string
}

// captureConfig is the device configuration: miniaudio converts whatever the
// hardware delivers to s16le 16 kHz mono.
func captureConfig() malgo.DeviceConfig {
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = mic.Channels
	cfg.SampleRate = mic.SampleRate

	return cfg
}

// cardOfID extracts the card number from an ALSA ID such as "hw:1,0" or
// "plughw:1,0". IDs that name the card symbolically do not resolve.
func cardOfID(id string) (int, bool) {
	_, rest, ok := strings.Cut(id, ":")
	if !ok {
		return 0, false
	}
	digits, _, _ := strings.Cut(rest, ",")
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return 0, false
	}

	return n, true
}

// selectDevice picks the device for rcv: an explicit CaptureDevice matches by
// name (or ID) substring, otherwise the ALSA ID must name the discovered card.
func selectDevice(devs []candidate, rcv mic.Receiver) (int, bool) {
	for i, d := range devs {
		if rcv.CaptureDevice != "" {
			if strings.Contains(d.Name, rcv.CaptureDevice) || strings.Contains(d.ID, rcv.CaptureDevice) {
				return i, true
			}

			continue
		}
		if card, ok := cardOfID(d.ID); ok && card == rcv.Card {
			return i, true
		}
	}

	return 0, false
}

// dataProc returns the audio-thread callback. It copies the frames (miniaudio
// reuses its buffer) and offers them without blocking.
func dataProc(q *recorder.FrameQueue) malgo.DataProc {
	return func(_, in []byte, _ uint32) {
		if len(in) == 0 {
			return
		}
		q.Offer(bytes.Clone(in))
	}
}

// stopProc returns the callback for an unexpected device stop. It reports
// the loss once, never blocking, and stays silent after a deliberate Close.
func stopProc(lost chan<- error, closed *atomic.Bool) malgo.StopProc {
	return func() {
		if closed.Load() {
			return
		}
		select {
		case lost <- fmt.Errorf("capture device stopped: %w", recorder.ErrDeviceLost):
		default:
		}
	}
}

// Open starts capturing from the receiver's card, or from the device named by
// rcv.CaptureDevice, into q.
func (*Capture) Open(rcv mic.Receiver, q *recorder.FrameQueue) (recorder.AudioStream, error) {
	ctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendAlsa}, malgo.ContextConfig{}, nil)
	if err != nil {
		return recorder.AudioStream{}, fmt.Errorf("init audio context: %w", err)
	}
	release := func() {
		_ = ctx.Uninit()
		ctx.Free()
	}

	id, err := pickID(ctx.Context, rcv)
	if err != nil {
		release()

		return recorder.AudioStream{}, err
	}

	lost := make(chan error, 1)
	closed := &atomic.Bool{}
	cfg := captureConfig()
	cfg.Capture.DeviceID = id.Pointer()
	// miniaudio copies the ID in InitDevice; Pointer allocates with C malloc.
	defer C.free(cfg.Capture.DeviceID)

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: dataProc(q),
		Stop: stopProc(lost, closed),
	})
	if err != nil {
		release()

		return recorder.AudioStream{}, fmt.Errorf("init capture device: %w", err)
	}
	err = dev.Start()
	if err != nil {
		dev.Uninit()
		release()

		return recorder.AudioStream{}, fmt.Errorf("start capture: %w", err)
	}

	return recorder.AudioStream{
		Lost: lost,
		Close: func() error {
			if closed.Swap(true) {
				return nil
			}
			dev.Uninit()
			release()

			return nil
		},
	}, nil
}

// deviceID returns the backend's ID string; for ALSA it is the NUL-terminated
// PCM name, not the hex form malgo's String prints.
func deviceID(id malgo.DeviceID) string {
	b := id[:]
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}

	return string(b)
}

// noDevice describes the failed selection without echoing device lists.
func noDevice(rcv mic.Receiver) error {
	if rcv.CaptureDevice != "" {
		return errors.New("no capture device matches the configured name")
	}

	return fmt.Errorf("no capture device for card %d", rcv.Card)
}

// pickID lists the capture devices and returns the ID of the one for rcv.
func pickID(ctx malgo.Context, rcv mic.Receiver) (malgo.DeviceID, error) {
	infos, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return malgo.DeviceID{}, fmt.Errorf("list capture devices: %w", err)
	}
	cands := make([]candidate, len(infos))
	for i := range infos {
		cands[i] = candidate{ID: deviceID(infos[i].ID), Name: infos[i].Name()}
	}
	idx, ok := selectDevice(cands, rcv)
	if !ok {
		return malgo.DeviceID{}, noDevice(rcv)
	}

	return infos[idx].ID, nil
}
