//go:build linux

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

// syncBuf886 is a bytes.Buffer an slog handler can write from any goroutine.
type syncBuf886 struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncBuf886) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncBuf886) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// standbyAborted886 reports whether the adaptive engine logged that it gave up
// a switch because the lazy standby could not be built.
func standbyAborted886(logs *syncBuf886) bool {
	for _, line := range strings.Split(logs.text(), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, "aborting switch: lazy standby build failed") {
			return true
		}
	}
	return false
}

// skipOrFailIfStandbyAborted886 is the guard of a test that forces a switch
// through the public surface and then judges that the active engine changed:
// where the engine logged that it aborted the switch for want of its standby,
// no switch was ever going to happen, so the test skips, or fails under
// CELERIS_REQUIRE_UPSWITCH=1 (the repo's gate for tests that need the up-switch).
// It keys on the engine's own record (the WARN and AdaptiveSwitches == 0) because
// the engine's slots are not reachable from this package.
func skipOrFailIfStandbyAborted886(t *testing.T, switches uint64, logs *syncBuf886) {
	t.Helper()
	if switches != 0 || !standbyAborted886(logs) {
		return
	}
	const msg = "the io_uring standby did not come up here: the engine aborted the forced switch and stayed put"
	if os.Getenv("CELERIS_REQUIRE_UPSWITCH") == "1" {
		t.Fatal(msg + " -- CELERIS_REQUIRE_UPSWITCH=1 forbids skipping")
	}
	t.Skip(msg)
}
