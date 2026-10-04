//go:build linux

package celeris_test

import (
	"testing"

	"github.com/goceleris/celeris"
)

// TestRouteDoesNotRunAfterAMiddlewareAnsweredNativeEngines927 is
// TestRouteDoesNotRunAfterAMiddlewareAnswered927 on the native engines.
func TestRouteDoesNotRunAfterAMiddlewareAnsweredNativeEngines927(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		t.Run(e.name, func(t *testing.T) { checkRouted927(t, e.eng) })
	}
}
