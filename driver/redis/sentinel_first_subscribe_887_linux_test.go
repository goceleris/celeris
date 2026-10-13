//go:build linux

package redis

// celeris#887 item 12, the in-repo caller: connectSentinel treats an error of
// its first SUBSCRIBE as "try the next sentinel". Client.Subscribe returns the
// PubSub (not ErrPubSubConnDown) when the conn is found down on the first
// SUBSCRIBE, and no look at the conn afterwards can tell every such case from
// a live conn, so connectSentinel makes the first SUBSCRIBE itself (newPubSub
// and PubSub.Subscribe) and decides from that error. A sentinel it accepted
// with a conn down would have its reconnect loop retry that one address for
// ever while watchFailover waits.
//
// The tests set testHookBeforeBind, so they must not run in parallel with
// another test that builds a PubSub.

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

func TestConnectSentinelMovesOnWhenTheFirstSubscribeFindsTheConnDown887(t *testing.T) {
	a := startFakeSentinel(t, "")
	b := startFakeSentinel(t, "")
	s := &SentinelClient{cfg: SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{a.Addr(), b.Addr()},
		DialTimeout:   2 * time.Second,
	}}

	// The first sentinel hangs up on the conn that the first Subscribe built,
	// and stays down: its listener is closed, so the PubSub's reconnect cannot
	// reach it again.
	var fired atomic.Bool
	setBeforeBindHook(func(c *redisConn) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		// The dial returns once the socket is connected, which can be before
		// the fake server's accept loop has recorded it: wait for it, and take
		// the lock only inside the condition (a panic with the lock held
		// would deadlock the fake's cleanup).
		var last net.Conn
		waitFor(t, "the first sentinel to accept the conn", 5*time.Second, func() bool {
			a.fake.mu.Lock()
			defer a.fake.mu.Unlock()
			if len(a.fake.conns) == 0 {
				return false
			}
			last = a.fake.conns[len(a.fake.conns)-1]
			return true
		})
		_ = a.fake.ln.Close()
		_ = last.Close()
		waitFor(t, "the loop to tear the new conn down (onClose)", 5*time.Second, c.closed.Load)
	})
	t.Cleanup(func() { testHookBeforeBind.Store(nil) })

	sc, err := s.connectSentinel()
	if err != nil {
		t.Fatalf("connectSentinel: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.subPS.Close()
		_ = sc.client.Close()
	})
	testHookBeforeBind.Store(nil)
	if !fired.Load() {
		t.Fatal("fixture: the before-bind hook did not run")
	}
	if sc.addr != b.Addr() {
		t.Fatalf("connectSentinel accepted %s (the sentinel that hung up and is down), want the next one, %s", sc.addr, b.Addr())
	}
}

// gateServer is the SentinelConfig.Engine of a test that wraps the standalone
// provider in the write gate of pubsub_ctlwrite_929_linux_test.go.
type gateServer struct{ p engine.EventLoopProvider }

func (s gateServer) EventLoopProvider() engine.EventLoopProvider { return s.p }

// A conn that is down but not yet marked closed. The loop's teardown marks the
// conn closed, forgets the descriptor, and only then runs onClose, which
// stores redisConn.closed; a write in between gets ErrUnknownFD while
// closed is still false. control() maps that to ErrPubSubConnDown, so the
// PubSub is not closed and its conn does not look closed: nothing but the
// error of the first control call itself says that the first SUBSCRIBE was
// not sent. connectSentinel must read that error: a sentinel it accepts here
// would be retried at this one address for ever while watchFailover sees no
// failover, where main moved on to the next sentinel.
func TestConnectSentinelMovesOnWhenTheFirstSubscribeFindsTheDescriptorForgotten887(t *testing.T) {
	a := startFakeSentinel(t, "")
	b := startFakeSentinel(t, "")
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eventloop.Release(prov) })
	g := newWriteGate()
	g.armFailWith(hasCmd("SUBSCRIBE"), ErrUnknownFD)
	s := &SentinelClient{cfg: SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{a.Addr(), b.Addr()},
		DialTimeout:   2 * time.Second,
		Engine:        gateServer{gateProvider{EventLoopProvider: prov, g: g}},
	}}
	// The conn of the first sentinel's PubSub, to see that the rejected
	// PubSub is closed and does not keep its conn (and a reconnect loop).
	var first atomic.Pointer[redisConn]
	setBeforeBindHook(func(c *redisConn) { first.CompareAndSwap(nil, c) })
	t.Cleanup(func() { testHookBeforeBind.Store(nil) })
	sc, err := s.connectSentinel()
	testHookBeforeBind.Store(nil)
	if err != nil {
		t.Fatalf("connectSentinel: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.subPS.Close()
		_ = sc.client.Close()
	})
	g.mu.Lock()
	still := g.armed
	g.mu.Unlock()
	if still {
		t.Fatal("fixture: the injected SUBSCRIBE failure never fired")
	}
	if sc.addr != b.Addr() {
		t.Fatalf("connectSentinel accepted %s, whose first SUBSCRIBE was not sent (ErrUnknownFD: the loop forgot the conn before onClose stored closed), want the next sentinel, %s", sc.addr, b.Addr())
	}
	if c := first.Load(); c == nil {
		t.Fatal("fixture: the before-bind hook did not see the first sentinel's conn")
	} else {
		waitFor(t, "the rejected sentinel's PubSub to release its conn", 5*time.Second, c.closed.Load)
	}
}
