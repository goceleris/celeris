package redis

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// celeris#1246: SentinelClient.Close returned while the background work (the
// sentinel subscription, a failover in progress) could still hold a Client it had
// just built. Opened WithEngine of a server with no event loop of its own, that
// Client holds a reference to the driver's standalone loop, so the reference
// outlived Close until the work noticed the close (TestSentinelClientWithEngine
// ReleasesTheStandaloneLoop1238 saw one left on windows-latest). The tests below
// hold such a Client open from a hook while Close is called, which makes the
// race certain instead of a matter of scheduling.

// lingerAfterCancel is how long the hook of holdLateClient keeps a Client open
// once Close has cancelled its dial, unless the test has sampled the count
// first. A Close that waits for the work returns only after the hook has let go,
// so for it the linger is a delay and the count it reads is 0; a Close that does
// not wait returns while the hook still holds the Client, and reads 1. The
// outcome does not depend on the order the goroutines run in; the linger only
// bounds how long the hook can hold.
const lingerAfterCancel = 300 * time.Millisecond

// holdLateClient arms testHookClientBuilt for the SentinelClients opened with
// srv: the first Client of kind the background work builds signals entered and
// then stays in the hook until release is closed or Close cancels its ctx, and
// after that until sampled is called or lingerAfterCancel passes. stop releases
// it at once and disarms the hook, and is safe to call twice.
func holdLateClient(t *testing.T, srv ServerProvider, kind string) (entered <-chan struct{}, sampled, stop func()) {
	t.Helper()
	enter := make(chan struct{})
	release := make(chan struct{})
	sample := make(chan struct{})
	var once sync.Once
	hook := func(s *SentinelClient, k string, ctx context.Context) {
		if s.cfg.Engine != srv || k != kind {
			return
		}
		held := false
		once.Do(func() { close(enter); held = true })
		if !held {
			return
		}
		select {
		case <-release:
			return
		case <-ctx.Done():
		}
		select {
		case <-release:
		case <-sample:
		case <-time.After(lingerAfterCancel):
		}
	}
	testHookClientBuilt.Store(&hook)
	var stopOnce, sampleOnce sync.Once
	sampled = func() { sampleOnce.Do(func() { close(sample) }) }
	stop = func() {
		stopOnce.Do(func() {
			testHookClientBuilt.Store(nil)
			close(release)
		})
	}
	t.Cleanup(stop)
	return enter, sampled, stop
}

// awaitClosed1246 fails the test unless ch is closed within the deadline: the
// deadline only bounds a hang, nothing is ordered by it.
func awaitClosed1246(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// pollUntil1246 polls cond until it holds or the deadline passes. The deadline
// only bounds a hang: cond decides, and polling is for a state some goroutine
// reaches on its own (the subscription connecting), not for an ordering.
func pollUntil1246(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitSubscribed1246 waits until the fake sentinel has recorded n
// +switch-master subscriptions (one per SentinelClient): a notification pushed
// before that reaches nobody. The SUBSCRIBE is written without waiting for the
// server, so the SentinelClient having connected does not say it.
func awaitSubscribed1246(t *testing.T, fs *fakeSentinel, n int) {
	t.Helper()
	pollUntil1246(t, "the sentinel to record the +switch-master subscription", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.subWriters) >= n
	})
}

// The subscription is still connecting its first sentinel (it has built the
// sentinel's Client) when Close is called: Close must not return with that
// Client open.
func TestSentinelCloseWithASentinelClientInFlight1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master.Addr())
			before := eventloop.StandaloneRefs()
			entered, sampled, stop := holdLateClient(t, srv, "sentinel")
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitClosed1246(t, entered, "the subscription to build its sentinel Client")
			if got := eventloop.StandaloneRefs(); got != before+2 {
				t.Errorf("open: standalone loop references = %d, want %d (master and sentinel Clients)", got, before+2)
			}
			_ = sc.Close()
			wantRefs(t, before, "closed with a sentinel Client in flight")
			sampled()
			stop()
			_ = sc.Close()
			wantRefs(t, before, "closed twice")
		})
	}
}

// Every Close that returns, the one that closes and one that comes in while it
// runs, returns with nothing left open.
func TestSentinelConcurrentCloseWithAClientInFlight1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master.Addr())
			before := eventloop.StandaloneRefs()
			entered, sampled, _ := holdLateClient(t, srv, "sentinel")
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitClosed1246(t, entered, "the subscription to build its sentinel Client")
			// refs[i] is what Close number i saw on return; wg and the slice
			// are the only things the goroutines share.
			var refs [4]int
			var wg sync.WaitGroup
			for i := range refs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = sc.Close()
					refs[i] = eventloop.StandaloneRefs()
				}()
			}
			wg.Wait()
			sampled()
			for i, got := range refs {
				if got != before {
					t.Errorf("Close %d returned with standalone loop references = %d, want %d", i, got, before)
				}
			}
		})
	}
}

// A failover is dialling the new master (its Client is built) when Close is
// called: Close must not return with that Client open.
func TestSentinelCloseWithAMasterClientInFlight1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master1 := startFakeMaster(t)
			master2 := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master1.Addr())
			before := eventloop.StandaloneRefs()
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			// The subscription has connected, so +switch-master reaches the
			// failover handler.
			awaitSubscribed1246(t, sentinel, 1)
			entered, sampled, stop := holdLateClient(t, srv, "master")
			sentinel.SetMaster(master2.Addr())
			awaitClosed1246(t, entered, "the failover to build the new master's Client")
			_ = sc.Close()
			wantRefs(t, before, "closed with a master Client in flight")
			sampled()
			stop()
		})
	}
}

// The failover finishes, then Close: the swapped-out master Client is closed
// once, by the failover, and the new one once, by Close. A second release would
// take the reference of another user of the loop (other, below).
func TestSentinelCloseAfterAFailoverReleasesEachClientOnce1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master1 := startFakeMaster(t)
			master2 := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master1.Addr())
			before := eventloop.StandaloneRefs()
			other, err := eventloop.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer eventloop.Release(other)
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			first, err := sc.getClient()
			if err != nil {
				t.Fatal(err)
			}
			awaitSubscribed1246(t, sentinel, 1)
			sentinel.SetMaster(master2.Addr())
			pollUntil1246(t, "the failover to swap the master Client", func() bool {
				c, err := sc.getClient()
				return err == nil && c != first
			})
			_ = sc.Close()
			wantRefs(t, before+1, "closed after a failover")
			_ = sc.Close()
			wantRefs(t, before+1, "closed twice")
		})
	}
}

// startHangServer1246 starts a hangServer1246 closed by the test's cleanup.
func startHangServer1246(t *testing.T) *hangServer1246 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &hangServer1246{ln: ln}
	go h.accept()
	t.Cleanup(h.close)
	return h
}

// hangServer1246 accepts TCP conns and never answers; close, in a cleanup, ends
// the dials still waiting on them.
type hangServer1246 struct {
	ln       net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	accepted atomic.Int32
}

func (h *hangServer1246) accept() {
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.conns = append(h.conns, c)
		h.mu.Unlock()
		h.accepted.Add(1)
	}
}

func (h *hangServer1246) close() {
	_ = h.ln.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		_ = c.Close()
	}
}

// closeWithin1246 runs Close of sc and fails the test, with a deadline that
// only bounds a hang, unless it returns.
func closeWithin1246(t *testing.T, sc *SentinelClient, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sc.Close()
	}()
	awaitClosed1246(t, done, "Close with "+what)
}

// dialModes1246 are the Engine settings a SentinelClient is opened with: none
// (the mini-loop of the driver), and the two servers that run on the driver's
// standalone loop (a Client in direct mode on darwin and windows).
func dialModes1246(t *testing.T) map[string]ServerProvider {
	m := map[string]ServerProvider{"no engine": nil}
	for name, srv := range loopServers(t) {
		m[name] = srv
	}
	return m
}

// Close cancels the dial of a failover whose master accepts the connection and
// never answers the handshake: with a DialTimeout of an hour, only the cancel can
// end the dial (the deadline is out of reach), so a Close that does not cancel
// it does not return within the test's bound. In direct mode the handshake reads
// a plain net.Conn, which does not honour a cancel unless the dial closes the
// conn on it.
func TestSentinelCloseCancelsAFailoverDialInTheHandshake1246(t *testing.T) {
	for name, srv := range dialModes1246(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master.Addr())
			hang := startHangServer1246(t)
			before := eventloop.StandaloneRefs()
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   time.Hour,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitSubscribed1246(t, sentinel, 1)
			sentinel.SetMaster(hang.ln.Addr().String())
			pollUntil1246(t, "the failover to dial the master", func() bool { return hang.accepted.Load() >= 1 })
			closeWithin1246(t, sc, "a failover dial in the handshake")
			wantRefs(t, before, "closed")
		})
	}
}

// proxyFirstThenHang1246 forwards its first accepted conn (the discovery of
// NewSentinelClient) to target and holds every later one open without answering:
// the subscription then waits in the handshake or the first SUBSCRIBE of a
// sentinel that does not answer.
func proxyFirstThenHang1246(t *testing.T, target string) (addr string, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	accepted = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			if accepted.Add(1) != 1 {
				continue
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				continue
			}
			mu.Lock()
			conns = append(conns, up)
			mu.Unlock()
			go func() { _, _ = io.Copy(up, c) }()
			go func() { _, _ = io.Copy(c, up) }()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String(), accepted
}

// Close returns, with every Client given back, while the subscription is on a
// sentinel that accepts and never answers, whatever the DialTimeout. This does not
// pin the cancel (the subscription's SUBSCRIBE is written without waiting for the
// reply, so it is not blocked, and Close ends it through closeCh): the cancel is
// pinned by the failover dial of the test above and by the direct-dial test. The
// discovery of NewSentinelClient is answered; the one that is not hangs there,
// which this test does not touch.
func TestSentinelCloseWithASubscriptionToAHungSentinel1246(t *testing.T) {
	for name, srv := range dialModes1246(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			good := startFakeSentinel(t, master.Addr())
			hung, accepted := proxyFirstThenHang1246(t, good.Addr())
			before := eventloop.StandaloneRefs()
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{hung},
				DialTimeout:   time.Hour,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			pollUntil1246(t, "the subscription to dial the hung sentinel", func() bool { return accepted.Load() >= 2 })
			closeWithin1246(t, sc, "a subscription to a hung sentinel")
			wantRefs(t, before, "closed")
		})
	}
}

// Two SentinelClients on the shared standalone loop: closing one, twice, leaves
// the other working and its failover intact, and both give the loop back.
func TestSentinelClientsShareTheStandaloneLoop1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			master2 := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master.Addr())
			before := eventloop.StandaloneRefs()
			cfg := SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			}
			a, err := NewSentinelClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			b, err := NewSentinelClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := a.Set(ctx, "k", "v", 0); err != nil {
				t.Fatal(err)
			}
			// Both subscriptions are on the fake before a closes: b's failover
			// notification below reaches b whenever it is sent.
			awaitSubscribed1246(t, sentinel, 2)
			if err := a.Close(); err != nil {
				t.Fatalf("Close a: %v", err)
			}
			if err := a.Close(); err != nil {
				t.Fatalf("Close a twice: %v", err)
			}
			if _, err := a.Get(ctx, "k"); err != ErrClosed {
				t.Fatalf("a.Get after Close: %v, want ErrClosed", err)
			}
			if v, err := b.Get(ctx, "k"); err != nil || v != "v" {
				t.Fatalf("b.Get after a closed: %q, %v", v, err)
			}
			// b's failover still works.
			first, err := b.getClient()
			if err != nil {
				t.Fatal(err)
			}
			sentinel.SetMaster(master2.Addr())
			pollUntil1246(t, "b's failover to swap the master Client", func() bool {
				c, err := b.getClient()
				return err == nil && c != first
			})
			if err := b.Set(ctx, "k3", "v3", 0); err != nil {
				t.Fatalf("b.Set after the failover: %v", err)
			}
			if err := b.Close(); err != nil {
				t.Fatalf("Close b: %v", err)
			}
			wantRefs(t, before, "both closed")
		})
	}
}

// A direct-mode dial whose ctx is cancelled in the handshake of a node that
// accepts and never answers ends at once, with the cancel as its error, whatever
// the DialTimeout. It needs no Engine, so it runs on every OS and pins the
// cancel of dialDirectRedisConn where no Client is in direct mode by default
// (celeris#1246).
func TestDirectDialEndsOnCancelInTheHandshake1246(t *testing.T) {
	hang := startHangServer1246(t)
	cfg := Config{Addr: hang.ln.Addr().String(), DialTimeout: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		c   *redisConn
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := dialDirectRedisConn(ctx, cfg)
		done <- result{c, err}
	}()
	// The node has the conn: the dial is in its handshake, or about to be (a
	// cancel that comes first is as well reported, and the mutant, with no
	// cancel, waits the hour out either way).
	pollUntil1246(t, "the node to accept the dial", func() bool { return hang.accepted.Load() >= 1 })
	cancel()
	select {
	case r := <-done:
		if r.c != nil {
			_ = r.c.Close()
			t.Error("the dial returned a conn")
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the direct dial did not end on cancel")
	}
}
