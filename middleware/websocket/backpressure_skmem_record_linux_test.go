//go:build linux

package websocket

import (
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestWSOSkmemRendering pins how SO_MEMINFO's nine counters read, in ss -m's
// order and letters.
func TestWSOSkmemRendering(t *testing.T) {
	var m [unix.SK_MEMINFO_VARS]uint32
	for i := range m {
		m[i] = uint32(i + 1)
	}
	if got, want := wsoSkmemString(m), "skmem:(r1,rb2,t3,tb4,f5,w6,o7,bl8,d9)"; got != want {
		t.Fatalf("wsoSkmemString = %q, want %q", got, want)
	}
}

// TestWSOSkmemReadsTheKernel reads SO_MEMINFO of both ends of a real loopback
// connection: the receiver holds unread bytes (rmem_alloc > 0) and both ends
// have buffers (rcvbuf and sndbuf > 0).
func TestWSOSkmemReadsTheKernel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	acc := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		acc <- c
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	srv := <-acc
	if srv == nil {
		t.Fatal("accept failed")
	}
	defer func() { _ = srv.Close() }()
	if _, err := cli.Write(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	// the bytes are queued on the server's receive side once the loopback delivered them
	var got string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		got = wsoCliSkmem(srv)
		if !strings.HasPrefix(got, "skmem:(r0,") {
			break
		}
	}
	if !strings.HasPrefix(got, "skmem:(") || strings.HasPrefix(got, "skmem:(r0,") {
		t.Fatalf("server end: %q, want skmem with rmem_alloc > 0 (1000 unread bytes)", got)
	}
	m := mustSkmem(t, srv)
	if m[unix.SK_MEMINFO_RCVBUF] == 0 || m[unix.SK_MEMINFO_SNDBUF] == 0 {
		t.Fatalf("server end: rcvbuf %d sndbuf %d, want both > 0", m[unix.SK_MEMINFO_RCVBUF], m[unix.SK_MEMINFO_SNDBUF])
	}
	if s := wsoCliSkmem(cli); !strings.HasPrefix(s, "skmem:(") {
		t.Fatalf("client end: %q", s)
	}
}

func mustSkmem(t *testing.T, c net.Conn) [unix.SK_MEMINFO_VARS]uint32 {
	t.Helper()
	var m [unix.SK_MEMINFO_VARS]uint32
	sc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var rerr error
	if err := sc.Control(func(fd uintptr) { m, rerr = wsoSkmemFD(int(fd)) }); err != nil || rerr != nil {
		t.Fatalf("SO_MEMINFO: %v %v", err, rerr)
	}
	return m
}

// TestWSOTCPMemRendering pins the sockstat and tcp_mem rendering, the
// pressure verdict in it, and the TcpExt pick.
func TestWSOTCPMemRendering(t *testing.T) {
	const sockstat = "sockets: used 120\nTCP: inuse 96 orphan 0 tw 3 alloc 100 mem 5000\nUDP: inuse 0 mem 0\n"
	cases := []struct {
		name, tcpMem string
		want         string
	}{
		{"below pressure", "4000 6000 8000\n", "below the pressure line"},
		{"above pressure", "1000 4500 8000\n", "ABOVE the pressure line"},
		{"above the hard limit", "1000 2000 3000\n", "AT OR ABOVE the hard limit"},
	}
	for _, tc := range cases {
		got := wsoTCPMemString(sockstat, tc.tcpMem)
		if !strings.Contains(got, "TCP: inuse 96 orphan 0 tw 3 alloc 100 mem 5000") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want the TCP line and %q", tc.name, got, tc.want)
		}
	}
	if got := wsoTCPMemString("", "garbage"); !strings.Contains(got, "TCP: absent") {
		t.Errorf("no sockstat: %q", got)
	}
	const netstat = "TcpExt: SyncookiesSent PruneCalled RcvPruned OfoPruned TCPMemoryPressures TCPRcvQDrop Other\n" +
		"TcpExt: 0 11 22 33 44 55 66\nIpExt: InNoRoutes\nIpExt: 9\n"
	got := wsoTCPExtString(netstat, "")
	if strings.Contains(got, "(+") {
		t.Errorf("tcpext %q has a rise without a baseline", got)
	}
	const base = "TcpExt: SyncookiesSent PruneCalled RcvPruned OfoPruned TCPMemoryPressures TCPRcvQDrop Other\n" +
		"TcpExt: 0 11 20 30 44 50 66\n"
	if d := wsoTCPExtString(netstat, base); !strings.Contains(d, "PruneCalled=11(+0)") ||
		!strings.Contains(d, "RcvPruned=22(+2)") || !strings.Contains(d, "OfoPruned=33(+3)") || !strings.Contains(d, "TCPRcvQDrop=55(+5)") {
		t.Errorf("tcpext with a baseline: %q, want each total with its rise", d)
	}
	for _, w := range []string{"PruneCalled=11", "RcvPruned=22", "OfoPruned=33", "TCPMemoryPressures=44", "TCPRcvQDrop=55"} {
		if !strings.Contains(got, w) {
			t.Errorf("tcpext %q lacks %q", got, w)
		}
	}
	if strings.Contains(got, "Other") || strings.Contains(got, "SyncookiesSent") {
		t.Errorf("tcpext %q holds a counter that is not a prune or drop one", got)
	}
	if got := wsoTCPExtString("nothing", ""); got != "tcpext{unavailable}" {
		t.Errorf("no netstat: %q", got)
	}
}

// TestWSORecordHasBothEndsAndTheKernelMemory builds a record on a real
// joined pair of sockets: the WSO-GIVEUP and WSO-CLOSESLOW records carry both
// ends' skmem and the kernel's TCP memory against tcp_mem (celeris#783), the
// slow-close record has the timeline (celeris#633), appears only over
// wsoCloseSlow, and does not mark the client as given up (that would excuse
// server errors serverErrs should judge).
func TestWSORecordHasBothEndsAndTheKernelMemory(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer func() { _ = c.Close() }()
			time.Sleep(3 * time.Second)
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	g := &wsoRig{t0: time.Now()}
	g.setServer(ln.Addr().String())
	cl := g.client(c)
	cl.mark("Close sent")
	cl.sample(c, "close wait")

	slow := wsoClose{outcome: "eof", dur: 12 * time.Second, maxGap: 11 * time.Second}
	rec, ok := cl.closeSlowRecord(c, "epoll", slow)
	if !ok {
		t.Fatal("a close wait with an 11 s silence has no WSO-CLOSESLOW record")
	}
	for _, want := range []string{"WSO-CLOSESLOW epoll conn=127.0.0.1:", "timeline (1 samples", "close wait", "now: cli skmem:(r", "now: srv skmem:", "now: kernel sockstat{TCP:", "tcp_mem{", "tcpext{", "maxGap=11s"} {
		if !strings.Contains(rec, want) {
			t.Errorf("record lacks %q:\n%s", want, rec)
		}
	}
	if cl.gaveUp {
		t.Fatal("a slow close marked the client as given up: that excuses server errors")
	}
	if _, ok := cl.closeSlowRecord(c, "epoll", wsoClose{outcome: "eof", dur: 12 * time.Second, maxGap: wsoCloseSlow}); ok {
		t.Fatal("a silence of exactly wsoCloseSlow is not over it")
	}
	if _, ok := cl.closeSlowRecord(c, "epoll", wsoClose{outcome: "eof", dur: 2 * time.Second, maxGap: time.Second}); ok {
		t.Fatal("a short close wait has a WSO-CLOSESLOW record")
	}
	give := cl.record(c, "WSO-GIVEUP", "epoll", "x")
	for _, want := range []string{"WSO-GIVEUP epoll", "now: cli skmem:(r", "now: kernel sockstat{TCP:", "tcp_mem{"} {
		if !strings.Contains(give, want) {
			t.Errorf("give-up record lacks %q:\n%s", want, give)
		}
	}
}
