package mic

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// Linux input constants the recorder cares about.
const (
	EvKey       = 1   // EV_KEY
	KeyVolumeUp = 115 // KEY_VOLUMEUP, what the receiver sends on a tap
)

// EventSize is the size in bytes of one 64-bit struct input_event record.
const EventSize = 24

// Event is a decoded input_event.
type Event struct {
	Sec, Usec int64
	Type      uint16
	Code      uint16
	Value     int32
}

// DecodeEvent decodes the 64-bit little-endian input_event layout: two
// 64-bit timeval words, then u16 type, u16 code and s32 value. Both
// supported targets (amd64, arm64) are 64-bit little-endian.
func DecodeEvent(b [EventSize]byte) Event {
	sec := binary.LittleEndian.Uint64(b[0:8])
	usec := binary.LittleEndian.Uint64(b[8:16])
	value := binary.LittleEndian.Uint32(b[20:24])

	// The conversions reinterpret two's-complement bits; overflow is intended.
	return Event{
		Sec:   int64(sec),  //nolint:gosec // G115: reinterpretation intended
		Usec:  int64(usec), //nolint:gosec // G115: reinterpretation intended
		Type:  binary.LittleEndian.Uint16(b[16:18]),
		Code:  binary.LittleEndian.Uint16(b[18:20]),
		Value: int32(value), //nolint:gosec // G115: reinterpretation intended
	}
}

// IsTap reports whether ev is a button press: EV_KEY, KEY_VOLUMEUP, value 1.
// Release (0) and autorepeat (2) are not taps.
func IsTap(ev Event) bool {
	return ev.Type == EvKey && ev.Code == KeyVolumeUp && ev.Value == 1
}

// HasKey reports whether bit code is set in a sysfs capabilities/key bitmap:
// space-separated hexadecimal 64-bit words, most significant word first.
// Bit N lives in word N/64 counted from the right. Malformed input yields
// false.
func HasKey(capabilitiesKeyHex string, code int) bool {
	if code < 0 {
		return false
	}
	words := strings.Fields(capabilitiesKeyHex)
	idx := len(words) - 1 - code/64
	if idx < 0 {
		return false
	}
	w, err := strconv.ParseUint(words[idx], 16, 64)
	if err != nil {
		return false
	}

	return w>>(uint(code)%64)&1 == 1
}
