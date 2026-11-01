//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#1165 and #1166 (found by the review of #1163): on a server whose
// clients are all HTTP/1, so that no loop holds an HTTP/2 conn, the shutdown
// must still do what net/http's Shutdown does and what the pool wait's gate
// does for a loop with an HTTP/2 conn:
//
//   - every idle keep-alive conn gets its FIN at once, whatever it has served:
//     a sync route, an Async route (a conn with a dispatch goroutine), or a sync
//     route whose request arrived in pieces on a server that has an Async route
//     (epoll promotes such a conn too), and whether or not other clients are
//     slowly reading big responses on the same loop (a send drain waiting for
//     them held these conns for the whole budget: #1165);
//   - a request in progress when the shutdown begins (a slow upload) is read on
//     and answered, with Connection: close, then its conn is closed (#1166).
//
// Workers 2 (the least a native engine takes) and the idle conns are spread over
// both; with downloads, six clients ask for 16 MiB and read nothing, so both
// loops hold one with p = 97%; without them no loop has anything to drain.
func TestShutdownIdleAndInProgressH1ConnsWithoutHTTP2_1165(t *testing.T) {
	engines := []struct {
		name string
		eng  celeris.EngineType
	}{
		{"std", celeris.Std},
		{"epoll", celeris.Epoll},
		{"io_uring", celeris.IOUring},
		{"adaptive", celeris.Adaptive},
	}
	for _, ec := range engines {
		for _, downloads := range []bool{true, false} {
			name := ec.name + "/no-downloads"
			if downloads {
				name = ec.name + "/slow-downloads"
			}
			t.Run(name, func(t *testing.T) { runShutdownH1Gate1165(t, ec.eng, downloads) })
		}
	}
}

func runShutdownH1Gate1165(t *testing.T, eng celeris.EngineType, downloads bool) {
	big := strings.Repeat("y", 16<<20)
	const upSize = 2 << 20
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, addr, done := startPickedCtx1104(ctx, t, celeris.Config{Engine: eng, Workers: 2, ShutdownTimeout: 20 * time.Second}, func(s *celeris.Server) {
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		s.GET("/big", func(c *celeris.Context) error { return c.String(http.StatusOK, "%s", big) })
		s.GET("/aping", func(c *celeris.Context) error { return c.String(http.StatusOK, "a") }).Async()
		s.POST("/up", func(c *celeris.Context) error { return c.String(http.StatusOK, "got %d", len(c.Body())) })
	}, func(_ *celeris.Server, addr string, done <-chan error) error { return waitDrainOrderReady(addr, done) })

	base := settledEngineMetrics1104Server(s)
	type idleConn struct {
		name string
		c    net.Conn
		br   *bufio.Reader
	}
	var idle []idleConn
	serve := func(c net.Conn, br *bufio.Reader, what string) {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s before the shutdown: %v", what, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	dial := func() net.Conn {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	for i := range 4 {
		for _, path := range []string{"/ping", "/aping"} {
			c := dial()
			br := bufio.NewReader(c)
			_, _ = io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n")
			serve(c, br, path)
			kind := "sync-route"
			if path == "/aping" {
				kind = "Async-route"
			}
			idle = append(idle, idleConn{kind + " " + strconv.Itoa(i), c, br})
		}
		// A sync route whose head arrives in two writes 30 ms apart.
		c := dial()
		_ = c.(*net.TCPConn).SetNoDelay(true)
		_, _ = io.WriteString(c, "GET /ping HTTP/1.1\r\nHo")
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(c, "st: x\r\n\r\n")
		br := bufio.NewReader(c)
		serve(c, br, "split /ping")
		idle = append(idle, idleConn{"split-head " + strconv.Itoa(i), c, br})
	}
	var dls []net.Conn
	if downloads {
		for range 6 {
			dl := dial()
			_, _ = io.WriteString(dl, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n")
			dls = append(dls, dl)
		}
	}
	// A request in progress: the head and the first 64 KiB of a 2 MiB body.
	up := dial()
	if _, err := io.WriteString(up, "POST /up HTTP/1.1\r\nHost: x\r\nContent-Length: "+strconv.Itoa(upSize)+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Write(make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	// The engine has accepted every conn before the shutdown begins: a conn still
	// in the accept queue (a loop busy formatting six 16 MiB responses under -race
	// has not got to the upload's) is reset by the listener's close, which says
	// nothing about the gate. Then the engine has read what the downloads and the
	// upload sent: a request whose head std's conn goroutine parses after the
	// shutdown began is not one in progress but a new one, which net/http drops (it
	// returns from the conn's goroutine once the head is parsed and the server is
	// shutting down, closing the conn with the body unread: an RST, celeris#1166).
	// epoll reads and answers a head still in the kernel at its first gate pass;
	// io_uring is not covered by this comment (not verified). The conn counts are
	// not enough (std counts a conn when it accepts it, before its goroutine has
	// run) and nor is a sleep: a loop busy under -race can be a few hundred ms late
	// to a conn. The wait is exact for the upload only: on std about 61 KiB of it
	// stay in the kernel until the handler reads the body, which is past the
	// shutting-down check. A download is a bare GET that std's first 4 KiB read
	// empties before that check, and on io_uring an empty receive queue means the
	// kernel handed the bytes over, not that the engine parsed them; for those the
	// sleep below covers the rest (no assertion reads the downloads).
	want := base + int64(len(idle)+len(dls)+1)
	for dl := time.Now().Add(20 * time.Second); s.EngineInfo().Metrics.ActiveConnections < want; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(dl) {
			t.Fatalf("the engine counts %d conns, want %d before the shutdown begins", s.EngineInfo().Metrics.ActiveConnections, want)
		}
	}
	waitServerReadAll1166(t, addr, append(append([]net.Conn(nil), dls...), up)...)
	time.Sleep(300 * time.Millisecond)

	begin := time.Now()
	shutErr := make(chan error, 1)
	go func() {
		sctx, scancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer scancel()
		shutErr <- s.Shutdown(sctx)
	}()

	// Every idle conn is read at the same time, each with its own wait.
	const finBy = 1500 * time.Millisecond
	type result struct {
		name string
		err  error
		at   time.Duration
	}
	res := make(chan result, len(idle))
	var wg sync.WaitGroup
	for _, k := range idle {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = k.c.SetReadDeadline(begin.Add(finBy))
			_, err := k.br.ReadByte()
			res <- result{k.name, err, time.Since(begin)}
		}()
	}

	wg.Wait()
	close(res)
	var late []string
	for r := range res {
		if r.err != io.EOF {
			late = append(late, r.name+": "+r.err.Error()+" at "+r.at.Round(time.Millisecond).String())
		}
	}
	if len(late) > 0 {
		t.Errorf("%d of %d idle conns had no FIN %v into the shutdown (celeris#1165):\n  %s", len(late), len(idle), finBy, strings.Join(late, "\n  "))
	}
	// The rest of the upload, sent once every idle conn has had its FIN: the
	// gate's pass (grace 250 ms, then it marks each conn it meets close-after
	// and closes the idle ones) has run on both loops by then, so the response is
	// written after it, whatever the load. (A response written before the pass
	// is a keep-alive one: the request was in flight when the shutdown began.)
	_ = up.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := up.Write(make([]byte, upSize-(64<<10))); err != nil {
		t.Errorf("the rest of the upload, sent %v into the shutdown: %v: a request in progress was cut (celeris#1166)", time.Since(begin).Round(time.Millisecond), err)
	} else {
		_ = up.SetReadDeadline(time.Now().Add(5 * time.Second))
		upBr := bufio.NewReader(up)
		resp, err := http.ReadResponse(upBr, nil)
		if err != nil {
			t.Errorf("the response to the upload: %v: a request in progress was not answered (celeris#1166)", err)
		} else {
			b, _ := io.ReadAll(resp.Body)
			if want := "got " + strconv.Itoa(upSize); string(b) != want {
				t.Errorf("the response to the upload: %q, want %q", b, want)
			}
			if !resp.Close {
				t.Errorf("the response to the upload, written after the shutdown began, has no Connection: close")
			}
			if _, err := upBr.ReadByte(); err != io.EOF {
				t.Errorf("after the response to the upload the conn read %v, want a FIN", err)
			}
		}
	}

	// The downloaders go; the shutdown ends well before its budget.
	for _, c := range dls {
		_ = c.Close()
	}
	for _, k := range idle {
		_ = k.c.Close()
	}
	select {
	case <-shutErr:
	case <-time.After(25 * time.Second):
		t.Error("Shutdown did not return")
	}
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		t.Error("the server did not stop")
	}
}

// settledEngineMetrics1104Server is the engine's open conns once they have not
// changed for 100 ms (the readiness probe's conn goes when its client does).
func settledEngineMetrics1104Server(s *celeris.Server) int64 {
	prev := s.EngineInfo().Metrics.ActiveConnections
	for range 100 {
		time.Sleep(100 * time.Millisecond)
		m := s.EngineInfo().Metrics.ActiveConnections
		if m == prev {
			return m
		}
		prev = m
	}
	return prev
}

// waitServerReadAll1166 returns once, for each of conns, everything the client
// has written is read by the server: the client's socket has no unacknowledged
// byte (tx_queue 0) and the server's socket has no unread one (rx_queue 0), as
// /proc/net/tcp has them. Loopback delivers a write before the call returns, so
// a server socket with an empty queue has had its bytes read. It is the point
// from which a request is in progress for the engine, however late the loop that
// serves the conn is to it (a head the engine reads after the shutdown began is a
// new request, not an upload in progress: see runShutdownH1Gate1165). Where
// /proc/net/tcp cannot be read the wait is skipped and the test keeps its
// sleep.
func waitServerReadAll1166(t *testing.T, srvAddr string, conns ...net.Conn) {
	t.Helper()
	_, sport, err := net.SplitHostPort(srvAddr)
	if err != nil {
		t.Fatal(err)
	}
	for dl := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		queues, err := procTCPQueues1166()
		if err != nil {
			t.Logf("no /proc/net/tcp, the wait for the server's reads is skipped: %v", err)
			return
		}
		pending := ""
		for _, c := range conns {
			cport := strconv.Itoa(c.LocalAddr().(*net.TCPAddr).Port)
			tx, ok1 := queues[[2]string{cport, sport}] // the client's socket: local port, remote port
			rx, ok2 := queues[[2]string{sport, cport}] // the server's socket
			if !ok1 || !ok2 || tx.tx != 0 || rx.rx != 0 {
				pending = fmt.Sprintf("client port %s: found %v/%v, unacknowledged %d, unread by the server %d", cport, ok1, ok2, tx.tx, rx.rx)
				break
			}
		}
		if pending == "" {
			return
		}
		if time.Now().After(dl) {
			t.Fatalf("the engine has not read what a client wrote 20 s before the shutdown begins: %s", pending)
		}
	}
}

type tcpQueues1166 struct{ tx, rx int64 }

// procTCPQueues1166 is the tx and rx queues of every IPv4 socket, keyed by
// (local port, remote port).
func procTCPQueues1166() (map[[2]string]tcpQueues1166, error) {
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return nil, err
	}
	out := make(map[[2]string]tcpQueues1166)
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		lp := f[1][strings.LastIndexByte(f[1], ':')+1:]
		rp := f[2][strings.LastIndexByte(f[2], ':')+1:]
		lport, err1 := strconv.ParseInt(lp, 16, 32)
		rport, err2 := strconv.ParseInt(rp, 16, 32)
		q := strings.SplitN(f[4], ":", 2)
		if err1 != nil || err2 != nil || len(q) != 2 {
			continue
		}
		tx, err3 := strconv.ParseInt(q[0], 16, 64)
		rx, err4 := strconv.ParseInt(q[1], 16, 64)
		if err3 != nil || err4 != nil {
			continue
		}
		out[[2]string{strconv.FormatInt(lport, 10), strconv.FormatInt(rport, 10)}] = tcpQueues1166{tx, rx}
	}
	return out, nil
}
