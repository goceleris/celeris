//go:build linux

package iouring

import (
	"testing"

	"github.com/goceleris/celeris/internal/mono/monotest"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1178. The engine's timeouts (idle, read, header, write, the HTTP/2
// stream deadline and the protocol detection) are read from the clock of
// internal/mono, which is monotonic: a step of the wall clock neither cuts a
// connection that is making progress nor holds one that is not. The scenarios are
// monotest's, run with the wall reading of the clock stepping by 75 s every 50 ms,
// forward and backward (see its package documentation).
func TestEngineTimeoutsIgnoreAStepOfTheWallClock1178(t *testing.T) {
	monotest.Run(t, func(t *testing.T, cfg resource.Config, h stream.Handler) string {
		r := startEngine806(t, h, func(c *resource.Config) {
			addr := c.Addr
			*c = cfg
			c.Addr = addr
		})
		return r.addr
	})
}
