//go:build linux

package redis

// celeris#887 item 10: the PubSub rig tests ran only on the standalone event
// loop (eventloop.Resolve(nil)); PubSub on the epoll and io_uring engines
// (WithEngine) was covered only by the Driver Conformance job against a live
// redis. The rig now takes the engine: the standalone loop, or the provider of
// a celeris.Server running the epoll or the io_uring engine, started the way
// test/integration's startSharedLoopServer does. Every rig test runs once per
// engine, as a subtest named after it.

import (
	"context"
	"net"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

type rigEngine string

const (
	rigStandalone rigEngine = "standalone"
	rigEpoll      rigEngine = "epoll"
	rigIOUring    rigEngine = "iouring"
)

var rigEngines = []rigEngine{rigStandalone, rigEpoll, rigIOUring}

// eachRigEngine runs fn as a subtest per engine.
func eachRigEngine(t *testing.T, fn func(t *testing.T, eng rigEngine)) {
	t.Helper()
	for _, eng := range rigEngines {
		t.Run(string(eng), func(t *testing.T) { fn(t, eng) })
	}
}

// rigProvider returns the EventLoopProvider of eng. The engine variants run a
// celeris.Server on a loopback listener (no routes) and use its provider; a
// variant the host cannot run (for example io_uring on a kernel without it)
// skips with the reason, which the test log shows.
func rigProvider(t *testing.T, eng rigEngine) engine.EventLoopProvider {
	t.Helper()
	if eng == rigStandalone {
		prov, err := eventloop.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { eventloop.Release(prov) })
		return prov
	}
	var kind celeris.EngineType
	switch eng {
	case rigEpoll:
		kind = celeris.Epoll
	case rigIOUring:
		kind = celeris.IOUring
	default:
		t.Fatalf("unknown rig engine %q", eng)
	}
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: kind})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListener(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Skipf("the %s engine cannot run on this host: %v", eng, err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		t.Fatalf("the %s server never bound within 10 s", eng)
	}
	prov := srv.EventLoopProvider()
	if prov == nil {
		t.Skipf("the %s server has no EventLoopProvider", eng)
	}
	// The variant must really run on the engine it names: a server that fell
	// back to another engine would make the subtest a second standalone run.
	if info := srv.EngineInfo(); info == nil || info.Type != kind {
		t.Fatalf("rig engine %s: the server runs %+v", eng, info)
	}
	t.Logf("rig engine %s: provider %T, %d workers", eng, prov, prov.NumWorkers())
	return prov
}
