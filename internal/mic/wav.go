package mic

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Recording format (research R5): 16 kHz, mono, 16-bit PCM, 32 000 B/s.
const (
	SampleRate = 16000
	Channels   = 1
	BitDepth   = 16
	// FrameSize is the size of one sample frame in bytes.
	FrameSize = Channels * BitDepth / 8
	// HeaderSize is the size of the canonical PCM WAV header written by Header.
	HeaderSize = 44

	wavFormatPCM = 1
	// maxDataBytes is the largest data size a 32-bit RIFF field can hold.
	maxDataBytes = 1<<32 - 1 - (HeaderSize - 8)
)

// ErrUnsupportedFormat is matched (errors.Is) by every error Parse and Repair
// return for input that is not the recording format. The upload handler maps
// it to HTTP 415.
var ErrUnsupportedFormat = errors.New("unsupported audio format")

// FormatError describes why a file is not a supported WAV. Use errors.As to
// read Reason; it satisfies errors.Is(err, ErrUnsupportedFormat).
type FormatError struct {
	Reason string
}

func (e *FormatError) Error() string { return "unsupported audio format: " + e.Reason }

// Is reports whether target is ErrUnsupportedFormat.
func (e *FormatError) Is(target error) bool { return target == ErrUnsupportedFormat }

func formatErr(format string, args ...any) error {
	return &FormatError{Reason: fmt.Sprintf(format, args...)}
}

// Header returns the 44-byte PCM WAV header for dataBytes of 16 kHz mono
// 16-bit audio. Header(0) is the placeholder written when a recording starts.
func Header(dataBytes int64) []byte {
	h := make([]byte, HeaderSize)
	le := binary.LittleEndian
	copy(h[0:], "RIFF")
	le.PutUint32(h[4:], u32(dataBytes+HeaderSize-8))
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], wavFormatPCM)
	le.PutUint16(h[22:], Channels)
	le.PutUint32(h[24:], SampleRate)
	le.PutUint32(h[28:], SampleRate*FrameSize)
	le.PutUint16(h[32:], FrameSize)
	le.PutUint16(h[34:], BitDepth)
	copy(h[36:], "data")
	le.PutUint32(h[40:], u32(dataBytes))

	return h
}

// u32 narrows v to the 32-bit RIFF size fields. Repair bounds recordings by
// maxDataBytes, so real callers never overflow.
func u32(v int64) uint32 {
	return uint32(v) //nolint:gosec // bounded by maxDataBytes
}

// Info is what Parse reads from a WAV header.
type Info struct {
	// DataBytes is the size declared by the data chunk header.
	DataBytes int64
}

// Parse reads the WAV header from r, accepting only PCM, 16-bit, mono,
// 16 kHz. Other chunks between fmt and data are skipped. It does not read the
// samples. Any other input yields a *FormatError (errors.Is
// ErrUnsupportedFormat); I/O errors other than a short read are wrapped as is.
func Parse(r io.Reader) (Info, error) {
	if err := readRIFF(r); err != nil {
		return Info{}, err
	}

	le := binary.LittleEndian
	sawFmt := false
	for {
		var ch [8]byte
		if err := readFull(r, ch[:]); err != nil {
			return Info{}, fmt.Errorf("read chunk header: %w", err)
		}
		id, size := string(ch[0:4]), int64(le.Uint32(ch[4:]))
		switch id {
		case "fmt ":
			if err := readFmt(r, size); err != nil {
				return Info{}, err
			}

			sawFmt = true
		case "data":
			if !sawFmt {
				return Info{}, formatErr("data chunk before fmt chunk")
			}
			return Info{DataBytes: size}, nil
		default:
			if err := skip(r, size+size&1); err != nil {
				return Info{}, err
			}
		}
	}
}

// readFmt reads and validates a fmt chunk of the given size, then skips any
// extension bytes and padding.
func readFmt(r io.Reader, size int64) error {
	if size < 16 {
		return formatErr("fmt chunk too short")
	}

	var f [16]byte
	if err := readFull(r, f[:]); err != nil {
		return fmt.Errorf("read fmt chunk: %w", err)
	}

	if err := checkFmt(f[:]); err != nil {
		return err
	}

	return skip(r, size-16+size&1)
}

func readRIFF(r io.Reader) error {
	var riff [12]byte
	if err := readFull(r, riff[:]); err != nil {
		return fmt.Errorf("read RIFF header: %w", err)
	}

	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return formatErr("not a RIFF/WAVE file")
	}

	return nil
}

func checkFmt(f []byte) error {
	le := binary.LittleEndian
	switch {
	case le.Uint16(f[0:]) != wavFormatPCM:
		return formatErr("not PCM (format tag %d)", le.Uint16(f[0:]))
	case le.Uint16(f[2:]) != Channels:
		return formatErr("want mono, got %d channels", le.Uint16(f[2:]))
	case le.Uint32(f[4:]) != SampleRate:
		return formatErr("want %d Hz, got %d Hz", SampleRate, le.Uint32(f[4:]))
	case le.Uint16(f[14:]) != BitDepth:
		return formatErr("want %d-bit, got %d-bit", BitDepth, le.Uint16(f[14:]))
	}

	return nil
}

// readFull reads len(b) bytes, mapping a short read to a FormatError.
func readFull(r io.Reader, b []byte) error {
	if _, err := io.ReadFull(r, b); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return formatErr("truncated header")
		}
		return fmt.Errorf("read WAV: %w", err)
	}

	return nil
}

func skip(r io.Reader, n int64) error {
	if n <= 0 {
		return nil
	}
	if _, err := io.CopyN(io.Discard, r, n); err != nil {
		if errors.Is(err, io.EOF) {
			return formatErr("truncated chunk")
		}
		return fmt.Errorf("skip chunk: %w", err)
	}

	return nil
}

// Repaired is the outcome of Repair: what the spool must apply to a
// .part.wav whose header was never finalised.
type Repaired struct {
	// Length is the length to truncate the file to: the header plus whole
	// frames only.
	Length int64
	// DataBytes is the sample byte count, a multiple of FrameSize.
	DataBytes int64
	// Header is the complete 44-byte header to write at offset 0.
	Header []byte
}

// Repair computes the corrected header for a recording file of fileLen bytes
// whose header holds a placeholder (research R6). A trailing partial frame is
// dropped. A file shorter than a header, or too large for WAV, yields a
// *FormatError.
func Repair(fileLen int64) (Repaired, error) {
	if fileLen < HeaderSize {
		return Repaired{}, formatErr("file of %d bytes is shorter than a WAV header", fileLen)
	}
	data := (fileLen - HeaderSize) / FrameSize * FrameSize
	if data > maxDataBytes {
		return Repaired{}, formatErr("recording of %d bytes exceeds the WAV size limit", data)
	}

	return Repaired{Length: HeaderSize + data, DataBytes: data, Header: Header(data)}, nil
}
