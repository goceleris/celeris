//go:build !unix

package postgres

import (
	"errors"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

// assertNumberReleased fails unless Close has handed c back to the loop: the
// conn holds a loop identifier, not a descriptor, so "released" means the loop
// no longer knows it (UnregisterConn reports an unknown fd) and has closed the
// net.Conn behind it.
func assertNumberReleased(t *testing.T, c *pgConn) {
	t.Helper()
	if err := c.loop.UnregisterConn(c.fd); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("expected ErrUnknownFD after Close(), got %v", err)
	}
}
