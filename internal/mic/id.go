package mic

import (
	"crypto/rand"
	"encoding/binary"
	"math"
)

// NewID returns a random positive int63 from crypto/rand (research R7). It
// panics if the system random source fails, which the platform treats as
// unrecoverable.
func NewID() int64 {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic("mic: crypto/rand failed: " + err.Error())
		}
		if v := int64(binary.BigEndian.Uint64(b[:]) & math.MaxInt64); v > 0 {
			return v
		}
	}
}
