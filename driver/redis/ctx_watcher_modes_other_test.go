//go:build !linux

package redis

import (
	"testing"

	"github.com/goceleris/celeris"
)

// ctxWatcherStartedModes: off Linux an engine that has not started has no loop
// of its own, so the Client runs direct.
func ctxWatcherStartedModes(t *testing.T) map[string][]Option {
	return map[string][]Option{
		"engine-not-started-std (direct)": {WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std}))},
	}
}
