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
// (n random bytes, hex-encoded). Uses a stack buffer for n<=32 (the common
// case) to avoid the intermediate byte slice allocation.
func HexToken(n int) string {
	if n > bufSize {
		out := make([]byte, n)
		if _, err := rand.Read(out); err != nil {
			panic("randutil: crypto/rand failed: " + err.Error())
		}
		return hex.EncodeToString(out)
	}
	var hexBuf [64]byte // stack buffer for tokens <= 32 bytes
	var dst []byte
	if n*2 <= len(hexBuf) {
		dst = hexBuf[:n*2]
	} else {
		dst = make([]byte, n*2)
	}
	mu.Lock()
	if pos+n > bufSize {
		fill()
	}
	hex.Encode(dst, buf[pos:pos+n])
	pos += n
	mu.Unlock()
	return string(dst)
}
