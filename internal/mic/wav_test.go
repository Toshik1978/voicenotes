package mic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestHeader(t *testing.T) {
	h := Header(1000)
	if len(h) != HeaderSize {
		t.Fatalf("len = %d", len(h))
	}
	if string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" || string(h[36:40]) != "data" {
		t.Fatalf("bad tags: %q", h)
	}
	if got := binary.LittleEndian.Uint32(h[4:]); got != 1036 {
		t.Errorf("riff size = %d", got)
	}
	if got := binary.LittleEndian.Uint32(h[24:]); got != SampleRate {
		t.Errorf("rate = %d", got)
	}
	if got := binary.LittleEndian.Uint32(h[28:]); got != 32000 {
		t.Errorf("byte rate = %d", got)
	}
	if got := binary.LittleEndian.Uint32(h[40:]); got != 1000 {
		t.Errorf("data size = %d", got)
	}
}

func TestParse(t *testing.T) {
	mod := func(off int, v uint16) []byte {
		h := Header(10)
		binary.LittleEndian.PutUint16(h[off:], v)
		return h
	}
	list := func() []byte { // fmt, LIST chunk (odd size, padded), data
		h := Header(8)
		var b bytes.Buffer
		b.Write(h[:36])
		b.WriteString("LIST")
		b.Write([]byte{3, 0, 0, 0, 'a', 'b', 'c', 0})
		b.Write(h[36:])

		return b.Bytes()
	}
	tests := []struct {
		name    string
		in      []byte
		want    int64
		wantErr bool
	}{
		{"ok", Header(10), 10, false},
		{"empty data chunk", Header(0), 0, false},
		{"placeholder header", Header(0), 0, false},
		{"odd length", Header(7), 7, false},
		{"extra chunk skipped", list(), 8, false},
		{"non-pcm", mod(20, 3), 0, true},
		{"stereo", mod(22, 2), 0, true},
		{"8 bit", mod(34, 8), 0, true},
		{
			"wrong rate",
			func() []byte { h := Header(1); binary.LittleEndian.PutUint32(h[24:], 44100); return h }(),
			0,
			true,
		},
		{"not riff", []byte("OggS000000000000000000000000000000000000000000000"), 0, true},
		{"truncated", Header(1)[:20], 0, true},
		{"empty", nil, 0, true},
		{"no data chunk", Header(1)[:36], 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := Parse(bytes.NewReader(tt.in))
			if tt.wantErr {
				var fe *FormatError
				if !errors.Is(err, ErrUnsupportedFormat) || !errors.As(err, &fe) || fe.Reason == "" {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.DataBytes != tt.want {
				t.Errorf("DataBytes = %d, want %d", info.DataBytes, tt.want)
			}
		})
	}
}

func TestRepair(t *testing.T) {
	tests := []struct {
		name     string
		fileLen  int64
		wantLen  int64
		wantData int64
		wantErr  bool
	}{
		{"empty data", 44, 44, 0, false},
		{"whole frames", 44 + 640, 684, 640, false},
		{"odd length", 44 + 641, 684, 640, false},
		{"one byte", 45, 44, 0, false},
		{"short header", 43, 0, 0, true},
		{"negative", -1, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Repair(tt.fileLen)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedFormat) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Length != tt.wantLen || r.DataBytes != tt.wantData {
				t.Errorf("got %+v", r)
			}
			if !bytes.Equal(r.Header, Header(tt.wantData)) {
				t.Error("header mismatch")
			}
			// A repaired placeholder file parses with the right size.
			file := append(append([]byte{}, r.Header...), make([]byte, r.DataBytes)...)
			info, err := Parse(bytes.NewReader(file))
			if err != nil || info.DataBytes != tt.wantData {
				t.Errorf("parse repaired: %v %+v", err, info)
			}
		})
	}
}
