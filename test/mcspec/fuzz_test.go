//go:build memcached

package mcspec

import (
	"errors"
	"runtime"
	"testing"

	"github.com/goceleris/celeris/driver/memcached/protocol"
)

// FuzzTextReaderRobustness feeds arbitrary bytes to a fresh TextReader and
// asserts it neither panics nor leaks goroutines. The driver's own readers
// are hardened; this target just nails down the guarantee from the outside.
func FuzzTextReaderRobustness(f *testing.F) {
	seeds := [][]byte{
		[]byte("STORED\r\n"),
		[]byte("NOT_STORED\r\n"),
		[]byte("EXISTS\r\n"),
		[]byte("NOT_FOUND\r\n"),
		[]byte("DELETED\r\n"),
		[]byte("TOUCHED\r\n"),
		[]byte("OK\r\n"),
		[]byte("END\r\n"),
		[]byte("ERROR\r\n"),
		[]byte("CLIENT_ERROR bad key\r\n"),
		[]byte("SERVER_ERROR oom\r\n"),
		[]byte("VERSION 1.6.24\r\n"),
		[]byte("VALUE k 0 5\r\nhello\r\nEND\r\n"),
		[]byte("VALUE k 0 5 42\r\nhello\r\nEND\r\n"),
		[]byte("STAT pid 1\r\nEND\r\n"),
		[]byte("42\r\n"),
		[]byte("VALUE k 0 999999999999999999\r\n"), // oversized bytes header
		[]byte("VALUE k 0 5\r\nhel"),               // short body
		[]byte("\r\n\r\n\r\n"),                     // blank lines
		[]byte{'\x00', '\x00', '\r', '\n'},
		[]byte("VALUE \x00key 0 1\r\nx\r\nEND\r\n"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	baselineGoroutines := runtime.NumGoroutine()

	f.Fuzz(func(t *testing.T, data []byte) {
		r := protocol.NewTextReader()
		r.Feed(data)
		for i := 0; i < 64; i++ {
			_, err := r.Next()
			if err != nil {
				if !errors.Is(err, protocol.ErrIncomplete) &&
					!errors.Is(err, protocol.ErrProtocol) {
					t.Fatalf("unexpected reader error: %v", err)
				}
				return
			}
		}
		// A well-formed stream may yield up to 64 replies without error; that
		// is also fine — we just want to guarantee no panic escaped.
		if got := runtime.NumGoroutine(); got > baselineGoroutines+4 {
			t.Fatalf("goroutine leak: baseline=%d now=%d", baselineGoroutines, got)
		}
	})
}
