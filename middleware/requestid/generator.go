package requestid

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
)

const uuidBytes = 16
const bufSize = uuidBytes * 256 // 4096 bytes = 256 UUIDs

type bufferedGenerator struct {
	mu  sync.Mutex
	buf [bufSize]byte
	pos int
}

func newBufferedGenerator() *bufferedGenerator {
	g := &bufferedGenerator{pos: bufSize}
	return g
}

func (g *bufferedGenerator) UUID() string {
	g.mu.Lock()
	if g.pos >= bufSize {
		if _, err := rand.Read(g.buf[:]); err != nil {
			panic("requestid: crypto/rand failed: " + err.Error())
		}
		g.pos = 0
	}
	var raw [uuidBytes]byte
	copy(raw[:], g.buf[g.pos:g.pos+uuidBytes])
	g.pos += uuidBytes
	g.mu.Unlock()

	// Set version 4 and variant bits.
	// Safe outside the critical section: raw is a local copy, not shared.
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // variant 10xx

	// Format: xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx
	var buf [36]byte
	hex.Encode(buf[0:8], raw[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], raw[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], raw[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], raw[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], raw[10:16])
	return string(buf[:])
}

// CounterGenerator returns a monotonic ID generator: "{prefix}-{counter}".
// Zero syscalls after init. For non-cryptographic use cases.
//
// The counter is process-local (in-memory atomic). Multiple processes
// or restarts will produce overlapping sequences. For cross-process
// uniqueness, include a process identifier in the prefix.
func CounterGenerator(prefix string) func() string {
	var counter atomic.Uint64
	return func() string {
		n := counter.Add(1)
		return prefix + "-" + formatUint(n)
	}
}

func formatUint(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
