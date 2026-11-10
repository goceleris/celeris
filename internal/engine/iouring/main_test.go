//go:build linux

package iouring

import (
	"os"
	"testing"

	"github.com/goceleris/celeris/internal/platform/pintest"
)

// TestMain only gives pintest.RunMain the process: it runs m.Run as it is,
// except in the child of TestStoppedEngineRestoresTheMainThreadMask890, whose
// main goroutine runs the worker on the main thread (celeris#890 item 42).
func TestMain(m *testing.M) {
	os.Exit(pintest.RunMain(m))
}
