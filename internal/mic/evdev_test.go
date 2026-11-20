package mic

import (
	"encoding/binary"
	"os"
	"testing"
)

func rawEvent(sec, usec int64, typ, code uint16, value int32) [24]byte {
	var b [24]byte
	binary.LittleEndian.PutUint64(b[0:], uint64(sec))
	binary.LittleEndian.PutUint64(b[8:], uint64(usec))
	binary.LittleEndian.PutUint16(b[16:], typ)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value))

	return b
}

func TestDecodeEvent(t *testing.T) {
	ev := DecodeEvent(rawEvent(1700000000, 123456, 1, 115, 1))
	want := Event{Sec: 1700000000, Usec: 123456, Type: 1, Code: 115, Value: 1}
	if ev != want {
		t.Fatalf("got %+v, want %+v", ev, want)
	}
	neg := DecodeEvent(rawEvent(0, 0, 3, 0x10, -5))
	if neg.Value != -5 || neg.Type != 3 || neg.Code != 0x10 {
		t.Fatalf("negative value: %+v", neg)
	}
	// Fixed bytes guard the layout independently of the helper.
	raw := [24]byte{16: 0x01, 18: 0x73, 20: 0x01}
	if got := DecodeEvent(raw); got.Type != EvKey || got.Code != KeyVolumeUp || got.Value != 1 {
		t.Fatalf("fixed bytes: %+v", got)
	}
}

func TestIsTap(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want bool
	}{
		{"press", Event{Type: 1, Code: 115, Value: 1}, true},
		{"release", Event{Type: 1, Code: 115, Value: 0}, false},
		{"autorepeat", Event{Type: 1, Code: 115, Value: 2}, false},
		{"other key", Event{Type: 1, Code: 114, Value: 1}, false},
		{"not a key event", Event{Type: 4, Code: 115, Value: 1}, false},
		{"sync", Event{Type: 0, Code: 0, Value: 0}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTap(tc.ev); got != tc.want {
				t.Fatalf("IsTap(%+v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func TestHasKey(t *testing.T) {
	tests := []struct {
		name string
		hex  string
		code int
		want bool
	}{
		{"consumer control fixture", readFixture(t, "key_consumer_control.txt"), 115, true},
		{"consumer control volume down", readFixture(t, "key_consumer_control.txt"), 114, true},
		{"consumer control no key 116", readFixture(t, "key_consumer_control.txt"), 116, false},
		{"keyboard fixture lacks 115", readFixture(t, "key_keyboard.txt"), 115, false},
		{"keyboard fixture has 30", readFixture(t, "key_keyboard.txt"), 30, true},
		{"keyboard fixture lacks 0", readFixture(t, "key_keyboard.txt"), 0, false},
		{"single short word, bit 3", "8", 3, true},
		{"single short word, bit 2", "8", 2, false},
		{"bit beyond words", "ffffffffffffffff", 64, false},
		{"bit 64 in second word", "1 0", 64, true},
		{"word order: ms first", "1 0", 0, false},
		{"upper case hex", "E000000000000 0", 115, true},
		{"extra whitespace", "  1   0\n", 64, true},
		{"empty", "", 115, false},
		{"only newline", "\n", 115, false},
		{"malformed word", "zz 0", 0, false},
		{"word over 64 bits", "10000000000000000", 0, false},
		{"negative code", "ff", -1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasKey(tc.hex, tc.code); got != tc.want {
				t.Fatalf("HasKey(%q, %d) = %v, want %v", tc.hex, tc.code, got, tc.want)
			}
		})
	}
}
