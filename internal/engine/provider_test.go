package engine

import "testing"

func TestProviderInterfaceShapes(t *testing.T) {
	t.Parallel()

	var provider EventLoopProvider
	var loop WorkerLoop
	_ = provider
	_ = loop
}
