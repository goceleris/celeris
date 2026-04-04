// Package randutil provides a shared, buffered cryptographic random token
// generator that amortizes crypto/rand syscalls across middleware packages.
package randutil

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

const bufSize = 4096

var (
	mu  sync.Mutex
	buf [bufSize]byte
	pos = bufSize // force initial fill
)

func fill() {
	if _, err := rand.Read(buf[:]); err != nil {
		panic("randutil: crypto/rand failed: " + err.Error())
	}
	pos = 0
}

// HexToken returns a cryptographically random hex string of n*2 characters
// (n random bytes, hex-encoded).
func HexToken(n int) string {
	out := make([]byte, n)
	if n > bufSize {
		if _, err := rand.Read(out); err != nil {
			panic("randutil: crypto/rand failed: " + err.Error())
		}
		return hex.EncodeToString(out)
	}
	mu.Lock()
	if pos+n > bufSize {
		fill()
	}
	copy(out, buf[pos:pos+n])
	pos += n
	mu.Unlock()
	return hex.EncodeToString(out)
}
