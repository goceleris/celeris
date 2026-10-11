package eventloop

import (
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

func TestResolveStandalone(t *testing.T) {
	resetStandaloneForTest()
	t.Cleanup(resetStandaloneForTest)

	p1, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	p2, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if p1 != p2 {
		t.Fatalf("standalone providers differ across Resolve: %p vs %p", p1, p2)
	}
	Release(p1)
	// Still one refcount outstanding, so the loop must survive.
	p3, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve #3: %v", err)
	}
	if p3 != p1 {
		t.Fatalf("standalone replaced before refcount drained")
	}
	Release(p2)
	Release(p3)

	// With refcount zero, the next Resolve should spawn a fresh loop.
	p4, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve #4: %v", err)
	}
	if p4 == p1 {
		t.Fatalf("expected new standalone after full release")
	}
	Release(p4)
}

type mockProvider struct{}

func (m *mockProvider) NumWorkers() int                    { return 1 }
func (m *mockProvider) WorkerLoop(n int) engine.WorkerLoop { return nil }

type fakeServer struct {
	p engine.EventLoopProvider
}

func (f *fakeServer) EventLoopProvider() engine.EventLoopProvider { return f.p }

func TestResolveServerPassthrough(t *testing.T) {
	resetStandaloneForTest()
	t.Cleanup(resetStandaloneForTest)

	mock := &mockProvider{}
	got, err := Resolve(&fakeServer{p: mock})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != engine.EventLoopProvider(mock) {
		t.Fatalf("Resolve returned %v, want mock provider", got)
	}
	// Release on a server-backed provider must be a no-op and must not
	// touch the standalone loop.
	Release(got)

	standaloneMu.Lock()
	if standalone != nil {
		standaloneMu.Unlock()
		t.Fatal("server-backed Resolve must not spawn a standalone loop")
	}
	standaloneMu.Unlock()
}

func TestResolveNilServer(t *testing.T) {
	resetStandaloneForTest()
	t.Cleanup(resetStandaloneForTest)

	// Server that reports no EventLoopProvider (e.g. net/http fallback).
	got, err := Resolve(&fakeServer{p: nil})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == nil {
		t.Fatal("expected standalone provider, got nil")
	}
	Release(got)
}
