package redis

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedMaster1104 is a Redis master whose reply to ROLE waits for release, so a
// test can call SentinelClient.Close while handleSwitchMaster's dialMaster is
// in flight (ROLE is the last step of the dial). role is what ROLE answers. It
// counts the ROLE requests it saw and the connections the client closed.
type gatedMaster1104 struct {
	ln      net.Listener
	role    string
	seen    chan struct{} // receives once per ROLE request (buffered)
	release chan struct{}
	roles   atomic.Int32
	closed  atomic.Int32
	mu      sync.Mutex
	conns   []net.Conn
}

func startGatedMaster1104(t *testing.T, role string) *gatedMaster1104 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &gatedMaster1104{ln: ln, role: role, seen: make(chan struct{}, 16), release: make(chan struct{})}
	t.Cleanup(func() {
		_ = ln.Close()
		g.mu.Lock()
		for _, c := range g.conns {
			_ = c.Close()
		}
		g.mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			g.mu.Lock()
			g.conns = append(g.conns, c)
			g.mu.Unlock()
			go g.serve(c)
		}
	}()
	return g
}

func (g *gatedMaster1104) Addr() string { return g.ln.Addr().String() }

func (g *gatedMaster1104) serve(c net.Conn) {
	defer func() { g.closed.Add(1); _ = c.Close() }()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	var buf []string
	for {
		cmd, err := readCommandInto(r, buf[:0])
		if err != nil {
			return
		}
		buf = cmd
		if len(cmd) == 0 {
			continue
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			proto := 2
			if len(cmd) > 1 && cmd[1] == "3" {
				proto = 3
			}
			handleHELLO(w, proto)
		case "ROLE":
			g.roles.Add(1)
			select {
			case g.seen <- struct{}{}:
			default:
			}
			<-g.release
			writeArrayHeader(w, 3)
			writeBulk(w, g.role)
			writeInt(w, 0)
			writeArrayHeader(w, 0)
		case "PING":
			writeSimple(w, "PONG")
		default:
			writeSimple(w, "OK")
		}
		if w.Flush() != nil {
			return
		}
	}
}

// switchPayload1104 is a +switch-master payload naming addr as the new master.
func switchPayload1104(addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	return "mymaster 127.0.0.1 1 " + host + " " + port
}

// closeDuringDial1104 runs handleSwitchMaster against g and calls Close while
// its dial is held at ROLE; it returns once handleSwitchMaster has returned.
func closeDuringDial1104(t *testing.T, g *gatedMaster1104) *SentinelClient {
	t.Helper()
	master := startFakeMaster(t)
	sent := startFakeSentinel(t, master.Addr())
	sc, err := NewSentinelClient(SentinelConfig{MasterName: "mymaster", SentinelAddrs: []string{sent.Addr()}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { sc.handleSwitchMaster(switchPayload1104(g.Addr())); close(done) }()
	select {
	case <-g.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the dial of the new master never reached ROLE")
	}
	_ = sc.Close()
	close(g.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handleSwitchMaster did not return")
	}
	return sc
}

// A Close that lands while handleSwitchMaster's dialMaster is in flight (it runs
// under its own timeout, not closeCh) must not leave the new client installed on
// the closed SentinelClient: nothing would ever close it. Before the fix
// handleSwitchMaster stored it with no look at the closed flag.
func TestSentinelCloseDuringSwitchMasterDialInstallsNothing1104(t *testing.T) {
	const iters = 20
	installed, open := 0, 0
	for range iters {
		g := startGatedMaster1104(t, "master")
		sc := closeDuringDial1104(t, g)
		sc.mu.RLock()
		if sc.client != nil {
			installed++
		}
		sc.mu.RUnlock()
		// The client the dial made is closed, not just left out.
		deadline := time.Now().Add(500 * time.Millisecond)
		for g.closed.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if g.closed.Load() == 0 {
			open++
		}
	}
	if installed != 0 || open != 0 {
		t.Errorf("after a Close during the dial: a client was installed on the closed SentinelClient in %d of %d runs, and its connection stayed open in %d", installed, iters, open)
	}
}

// A failed dial followed by a Close, with the retry pause over as well (a base of
// 1 ns), is a tie between closeCh and the timer; Go picks at random, so the
// loop dialled the master again after Close about half of the time. It must
// not: exactly one ROLE request, the one that was in flight when Close ran.
func TestSentinelSwitchMasterDoesNotDialAfterClose1104(t *testing.T) {
	old := sentinelSwitchBackoffBase
	sentinelSwitchBackoffBase = time.Nanosecond
	t.Cleanup(func() { sentinelSwitchBackoffBase = old })
	const iters = 100
	extra := 0
	for range iters {
		g := startGatedMaster1104(t, "slave") // the dial fails: not a master
		_ = closeDuringDial1104(t, g)
		if n := g.roles.Load(); n != 1 {
			extra++
		}
	}
	if extra != 0 {
		t.Errorf("handleSwitchMaster dialled the new master again after Close in %d of %d runs", extra, iters)
	}
}

// A +switch-master that was already queued when Close ran: watchFailover's
// select between the message and closeCh picks at random, so handleSwitchMaster
// can run after Close, and it dialled the new master once before it looked at
// anything. It must make no dial at all: no ROLE request, nothing installed. The
// test calls it after Close, which is the state the lost pick leaves.
func TestSentinelSwitchMasterAfterCloseMakesNoDial1104(t *testing.T) {
	const iters = 50
	dialled, installed := 0, 0
	for range iters {
		g := startGatedMaster1104(t, "master")
		close(g.release) // a ROLE would be answered at once
		master := startFakeMaster(t)
		sent := startFakeSentinel(t, master.Addr())
		sc, err := NewSentinelClient(SentinelConfig{MasterName: "mymaster", SentinelAddrs: []string{sent.Addr()}, DialTimeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		_ = sc.Close()
		sc.handleSwitchMaster(switchPayload1104(g.Addr()))
		// handleSwitchMaster dials synchronously, so a dial has reached ROLE by now.
		if g.roles.Load() != 0 {
			dialled++
		}
		sc.mu.RLock()
		if sc.client != nil {
			installed++
		}
		sc.mu.RUnlock()
	}
	if dialled != 0 || installed != 0 {
		t.Errorf("handleSwitchMaster on a closed SentinelClient: dialled the new master in %d of %d runs, installed a client in %d", dialled, iters, installed)
	}
}
