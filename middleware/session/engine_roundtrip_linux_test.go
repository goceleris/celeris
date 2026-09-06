//go:build linux

package session

import (
	"testing"

	"github.com/goceleris/celeris"
)

// TestLoginRoundTripNativeEngines runs the login -> cookie -> authenticated
// GET -> logout round trip against the io_uring and epoll engines, mirroring
// probatorium's auth_session_ratelimit refapp. Both engines materialise the
// response headers on the wire the moment the handler calls c.JSON, which is
// exactly the path that dropped the post-chain Set-Cookie. Skips when the
// engine is unavailable (seccomp-filtered or pre-5.1 kernels).
func TestLoginRoundTripNativeEngines(t *testing.T) {
	if testing.Short() {
		t.Skip("native engine round trip skipped in -short")
	}
	cases := []struct {
		name   string
		engine celeris.EngineType
	}{
		{"iouring", celeris.IOUring},
		{"epoll", celeris.Epoll},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runLoginRoundTrip(t, tc.engine)
		})
	}
}
