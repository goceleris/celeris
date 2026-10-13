//go:build linux

package redis

// celeris#887 item 12, the in-repo caller: connectSentinel treats an error of
// its first Subscribe as "try the next sentinel". Since Client.Subscribe
// returns the PubSub (not ErrPubSubConnDown) when the conn is found down on
// the first SUBSCRIBE, connectSentinel must look at the PubSub itself, or it
// accepts a sentinel whose conn is down and whose reconnect loop retries that
// one address for ever.
//
// The test sets testHookBeforeBind, so it must not run in parallel with
// another test that builds a PubSub.

import (
	"sync/atomic"
	"testing"
	"time"
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
	testHookBeforeBind = func(c *redisConn) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		_ = a.fake.ln.Close()
		a.fake.mu.Lock()
		last := a.fake.conns[len(a.fake.conns)-1]
		a.fake.mu.Unlock()
		_ = last.Close()
		waitFor(t, "the loop to tear the new conn down (onClose)", 5*time.Second, c.closed.Load)
	}
	t.Cleanup(func() { testHookBeforeBind = nil })

	sc, err := s.connectSentinel()
	if err != nil {
		t.Fatalf("connectSentinel: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.subPS.Close()
		_ = sc.client.Close()
	})
	testHookBeforeBind = nil
	if !fired.Load() {
		t.Fatal("fixture: the before-bind hook did not run")
	}
	if sc.addr != b.Addr() {
		t.Fatalf("connectSentinel accepted %s (the sentinel that hung up and is down), want the next one, %s", sc.addr, b.Addr())
	}
}
