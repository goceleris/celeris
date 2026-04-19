//go:build memcached

// zz_flush_test.go — placed in a file whose name sorts last so the flushing
// tests run after every other test in the package. memcached's flush_all
// stamps pre-existing items with an expiry at "now + delay"; on some
// builds the stamp briefly affects items created just after the command,
// which would corrupt adjacent text- and binary-protocol fixture data.
package mcspec

import (
	"testing"

	"github.com/goceleris/celeris/driver/memcached/protocol"
)

// TestZZBinary_Flush exercises OpFlush both with and without exptime extras.
// Deliberately the last test in the binary suite so pre-existing items set
// by other tests do not get retroactively invalidated.
func TestZZBinary_Flush(t *testing.T) {
	c := dialBinary(t)

	// No extras — immediate flush.
	c.writer.Reset()
	c.writer.AppendFlush(0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpFlush, protocol.StatusOK)

	// With exptime extras (schedule at a later moment).
	c.writer.Reset()
	c.writer.AppendFlush(1, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpFlush, protocol.StatusOK)
}
