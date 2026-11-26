//go:build linux

package redis

import (
	"testing"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
)

// harshEngineOptions makes the client engine-backed: a started epoll engine,
// whose command conns read on the caller's goroutine.
func harshEngineOptions(t *testing.T) []Option {
	return []Option{WithEngine(busypolltest.StartServer(t, 0))}
}
