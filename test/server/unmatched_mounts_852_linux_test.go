//go:build linux

package celeris_test

import (
	"testing"

	"github.com/goceleris/celeris"
)

// TestUseMountedMiddlewaresAnswerTheirPathsNativeEngines852 is
// TestUseMountedMiddlewaresAnswerTheirPaths852 on the native engines: the
// not-found and 405 chains are built once by Start and run on the engine's
// dispatch path.
func TestUseMountedMiddlewaresAnswerTheirPathsNativeEngines852(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		t.Run(e.name, func(t *testing.T) { checkUseMounts852(t, e.eng) })
	}
}
