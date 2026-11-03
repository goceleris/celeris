package eventloop

import (
	"sync"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// ServerProvider is implemented by *celeris.Server. The indirection avoids a
// dependency cycle (celeris root imports engine; driver/internal/eventloop
// cannot import celeris). Callers cast *celeris.Server to this interface.
type ServerProvider interface {
	// EventLoopProvider returns the server engine's event-loop provider, or
	// nil when the engine does not expose one (e.g. the net/http fallback).
	EventLoopProvider() engine.EventLoopProvider
}

// AsyncHandlerProvider is optionally implemented by ServerProvider to report
// whether the server dispatches HTTP handlers to spawned goroutines rather
// than running them inline on LockOSThread'd worker Gs. Drivers opened
// WithEngine(srv) consult this via type assertion — when the caller will
// run on an unlocked handler G, the direct net.Conn path becomes the
// fastest choice (Go netpoll parks the G on EPOLLIN without blocking an M).
// When the caller runs on a locked worker, the mini-loop busy-poll path
// is preferred (direct net.Conn.Read on a locked M triggers
// stoplockedm+startlockedm futex storms).
type AsyncHandlerProvider interface {
	AsyncHandlers() bool
}

// IsAsyncServer reports whether sp opts into async handler dispatch. Safe
// to call with a nil sp — returns false.
func IsAsyncServer(sp ServerProvider) bool {
	if sp == nil {
		return false
	}
	if a, ok := sp.(AsyncHandlerProvider); ok {
		return a.AsyncHandlers()
	}
	return false
}

// BusyPollProvider is optionally implemented by ServerProvider to report the
// server's resolved Config.SocketBusyPoll, the SO_BUSY_POLL budget its native
// engines put on the connections they accept (0 = off). Drivers opened
// WithEngine(srv) consult it via type assertion, so a driver socket registered
// on the server's loops follows the server's decision (celeris#1228). A
// ServerProvider that does not implement it reports off.
type BusyPollProvider interface {
	SocketBusyPoll() time.Duration
}

// BusyPoll is the SO_BUSY_POLL budget a driver puts on a connection it dials:
// explicit, the driver's own option, when it is at least one microsecond (the
// kernel takes whole microseconds, so anything less is "not set", and cannot
// silently turn off a server that has it on), else the budget sp reports, else
// 0 (off, the default, which means no setsockopt call). Safe to call with a nil
// sp.
func BusyPoll(sp ServerProvider, explicit time.Duration) time.Duration {
	if explicit >= time.Microsecond {
		return explicit
	}
	if sp == nil {
		return 0
	}
	if b, ok := sp.(BusyPollProvider); ok {
		return b.SocketBusyPoll()
	}
	return 0
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

// IsStandalone reports whether provider is the standalone Loop, that is, whether
// the Resolve that returned it took a reference that only a Release gives back.
// A provider that came from a Server is not: the Server owns its lifetime.
func IsStandalone(provider engine.EventLoopProvider) bool {
	if provider == nil {
		return false
	}
	standaloneMu.Lock()
	defer standaloneMu.Unlock()
	return standalone != nil && provider == engine.EventLoopProvider(standalone)
}

// StandaloneRefs is the number of references held on the standalone Loop, 0
// when none is running. It is for tests, which assert that every Resolve of the
// standalone Loop is paired with one Release (celeris#1238).
func StandaloneRefs() int {
	standaloneMu.Lock()
	defer standaloneMu.Unlock()
	return standaloneRef
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
