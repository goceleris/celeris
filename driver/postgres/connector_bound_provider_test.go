package postgres

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// A server whose engine runs handlers inline on its workers and has no
// synchronous round trip for a driver (a started epoll, io_uring or adaptive
// server, as far as Connect can tell): the loops it exposes.
type noSyncProvider struct{}

func (noSyncProvider) NumWorkers() int { return 1 }
func (noSyncProvider) WorkerLoop(int) engine.WorkerLoop {
	return noSyncLoop{}
}

// noSyncLoop is a WorkerLoop that does not implement syncRoundTripper; it is never
// called, a direct dial does not use it.
type noSyncLoop struct{ engine.WorkerLoop }

type boundServer struct{ p engine.EventLoopProvider }

func (s boundServer) EventLoopProvider() engine.EventLoopProvider { return s.p }

// The loops WithEngine took from a server survive every option that returns a
// new Connector, in every order; WithEngine itself replaces them, also with
// nothing (a nil server, or one that exposes no loops).
func TestConnectorChainKeepsTheBoundProvider(t *testing.T) {
	prov := noSyncProvider{}
	base := newFakeConnector(t, "")
	bound := base.WithEngine(boundServer{prov})
	if bound.provider != engine.EventLoopProvider(prov) || bound.ownsLoop {
		t.Fatalf("WithEngine did not bind the server's loops: provider=%v owns=%v", bound.provider, bound.ownsLoop)
	}
	const cap1 = 1 << 20
	opts := map[string]func(*Connector) *Connector{
		"WithStreaming":      func(c *Connector) *Connector { return c.WithStreaming() },
		"WithMaxResultBytes": func(c *Connector) *Connector { return c.WithMaxResultBytes(cap1) },
		"WithSocketBusyPoll": func(c *Connector) *Connector { return c.WithSocketBusyPoll(29 * time.Microsecond) },
	}
	var names []string
	for n := range opts {
		names = append(names, n)
	}
	for _, a := range names {
		if got := opts[a](bound).provider; got != engine.EventLoopProvider(prov) {
			t.Errorf("bound.%s lost the provider (got %v)", a, got)
		}
		for _, b := range names {
			if got := opts[b](opts[a](bound)).provider; got != engine.EventLoopProvider(prov) {
				t.Errorf("bound.%s.%s lost the provider (got %v)", a, b, got)
			}
		}
	}
	for name, cc := range map[string]*Connector{
		"bound.WithEngine(nil)":                    bound.WithEngine(nil),
		"bound.WithEngine(server with no loops)":   bound.WithEngine(boundServer{nil}),
		"bound.WithMaxResultBytes.WithEngine(nil)": bound.WithMaxResultBytes(cap1).WithEngine(nil),
	} {
		if cc.provider != nil || cc.ownsLoop {
			t.Errorf("%s kept the old provider: %v owns=%v", name, cc.provider, cc.ownsLoop)
		}
	}
	if bound.provider != engine.EventLoopProvider(prov) {
		t.Error("an option changed the Connector it was called on")
	}
}

// A WithStreaming Connector bound to a server like that dials direct and the cap
// reaches the connection, whatever the order of WithStreaming and
// WithMaxResultBytes after WithEngine: it must not fall back to the driver's own
// loop (it would stream, with no cap, on the loop a nested query can stall on).
func TestConnectorBoundToAnInlineServerIsDirectWithTheCap(t *testing.T) {
	const n = 1 << 20
	bound := newFakeConnector(t, "").WithEngine(boundServer{noSyncProvider{}})
	for name, cn := range map[string]*Connector{
		"WithStreaming.WithMaxResultBytes": bound.WithStreaming().WithMaxResultBytes(n),
		"WithMaxResultBytes.WithStreaming": bound.WithMaxResultBytes(n).WithStreaming(),
	} {
		c := connectPG(t, cn)
		if !c.useDirect {
			t.Errorf("%s: the connection is on a loop, want direct", name)
		}
		if c.opts.maxResultBytes != n {
			t.Errorf("%s: the connection's cap = %d, want %d", name, c.opts.maxResultBytes, n)
		}
		cn.provMu.Lock()
		prov, owns := cn.provider, cn.ownsLoop
		cn.provMu.Unlock()
		if prov != engine.EventLoopProvider(noSyncProvider{}) || owns {
			t.Errorf("%s: Connect replaced the bound provider with %v (owns=%v)", name, prov, owns)
		}
	}
}

// Every method of Connector that returns a new Connector (a With* method) is in
// the chain test above: clone() does not copy the provider, so each one has to
// keep (or, like WithEngine, replace) it itself, and one that forgets it silently
// moves a Connector bound to a started server onto the driver's own loop. A new
// With* method fails here until it is added to that test.
func TestConnectorEveryWithMethodIsInTheChainTest(t *testing.T) {
	covered := map[string]bool{
		"WithEngine":         true, // replaces the provider (TestConnectorChainKeepsTheBoundProvider)
		"WithMaxResultBytes": true, // keeps it
		"WithSocketBusyPoll": true, // keeps it
		"WithStreaming":      true, // keeps it
	}
	typ := reflect.TypeOf(&Connector{})
	seen := map[string]bool{}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !strings.HasPrefix(name, "With") {
			continue
		}
		seen[name] = true
		if !covered[name] {
			t.Errorf("Connector.%s is not in TestConnectorChainKeepsTheBoundProvider: add it there, and make it keep the provider WithEngine bound (clone() does not copy it)", name)
		}
	}
	for name := range covered {
		if !seen[name] {
			t.Errorf("%s is listed as covered but Connector has no such method", name)
		}
	}
}
