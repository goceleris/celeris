package eventloop

import (
	"sync"

	"github.com/goceleris/celeris/engine"
)

// ServerProvider is implemented by *celeris.Server. The indirection avoids a
// dependency cycle (celeris root imports engine; driver/internal/eventloop
// cannot import celeris). Callers cast *celeris.Server to this interface.
type ServerProvider interface {
	// EventLoopProvider returns the server engine's event-loop provider, or
	// nil when the engine does not expose one (e.g. the net/http fallback).
	EventLoopProvider() engine.EventLoopProvider
}

// standaloneState tracks the package-level refcounted standalone Loop shared
// by drivers when no HTTP server is available.
var (
	standaloneMu  sync.Mutex
	standaloneRef int
	standalone    *Loop
)

// Resolve returns an [engine.EventLoopProvider] suitable for driver use.
//
// If sp is non-nil and its engine implements EventLoopProvider, that provider
// is returned. Otherwise Resolve spawns (or reuses) a package-level standalone
// Loop and increments its refcount. Server-backed providers are not
// refcounted — the Server owns its engine's lifetime.
//
// Every successful Resolve must be paired with a Release so the standalone
// Loop can be torn down when the last driver releases it.
func Resolve(sp ServerProvider) (engine.EventLoopProvider, error) {
	if sp != nil {
		if p := sp.EventLoopProvider(); p != nil {
			return p, nil
		}
	}
	standaloneMu.Lock()
	defer standaloneMu.Unlock()
	if standalone == nil {
		l, err := New(0)
		if err != nil {
			return nil, err
		}
		standalone = l
	}
	standaloneRef++
	return standalone, nil
}

// Release decrements the refcount for the standalone Loop. If the provider
// was not the standalone Loop (e.g. it came from a Server) the call is a
// no-op. When the last driver releases the standalone Loop, it is closed.
func Release(provider engine.EventLoopProvider) {
	if provider == nil {
		return
	}
	standaloneMu.Lock()
	if standalone == nil || provider != engine.EventLoopProvider(standalone) {
		standaloneMu.Unlock()
		return
	}
	standaloneRef--
	var toClose *Loop
	if standaloneRef <= 0 {
		standaloneRef = 0
		toClose = standalone
		standalone = nil
	}
	standaloneMu.Unlock()
	if toClose != nil {
		_ = toClose.Close()
	}
}

// resetStandaloneForTest tears down any cached standalone loop. Intended for
// tests that need a clean refcount between cases; not part of the public API.
func resetStandaloneForTest() {
	standaloneMu.Lock()
	l := standalone
	standalone = nil
	standaloneRef = 0
	standaloneMu.Unlock()
	if l != nil {
		_ = l.Close()
	}
}
