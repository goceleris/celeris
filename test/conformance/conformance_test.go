// Package conformance provides cross-engine protocol conformance tests.
package conformance

import (
	"testing"

	"github.com/goceleris/celeris/engine"
)

var protocols = []engine.Protocol{
	engine.HTTP1,
	engine.H2C,
	engine.Auto,
}

func TestConformanceMatrix(t *testing.T) {
	for _, ef := range Factories() {
		if !ef.Available() {
			t.Logf("skipping %s (not available)", ef.Name)
			continue
		}
		for _, proto := range protocols {
			name := ef.Name + "/" + proto.String()
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				runConformanceSuite(t, ef, proto)
			})
		}
	}
}

func runConformanceSuite(t *testing.T, ef EngineFactory, proto engine.Protocol) {
	t.Run("BasicMethods", func(t *testing.T) {
		testBasicMethods(t, ef, proto)
	})
	t.Run("LargeBody", func(t *testing.T) {
		testLargeBody(t, ef, proto)
	})
	t.Run("KeepAlive", func(t *testing.T) {
		testKeepAlive(t, ef, proto)
	})
	t.Run("ErrorHandling", func(t *testing.T) {
		testErrorHandling(t, ef, proto)
	})
}
