//go:build linux

package stallprobe

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestProbeSnapshotSelfTest proves the probes' own machinery, with no celeris
// in it: a stub HTTP server that delays on command drives the large-response
// client through (1) a healthy transfer (no snapshot may fire: no false
// failure), (2) a delay before the response head and (3) a delay in the middle
// of the body, both longer than the snapshot threshold and shorter than the
// 5 s rule (a snapshot fires, the repetition PASSES), and (4) a stall that
// never ends (the repetition FAILS at the idle cap, with the snapshot digest in
// the failure message). It is not in any dispatch regexp of the probes; run it
// with -run '^TestProbeSnapshotSelfTest$'.
func TestProbeSnapshotSelfTest(t *testing.T) {
	old := pcfg
	defer func() { pcfg = old }()
	pcfg.SnapAfter, pcfg.SnapGap, pcfg.Slow = time.Second, 300*time.Millisecond, 500*time.Millisecond
	c := newClk()
	stwLeafBegin()

	// A thread that stands in for an engine loop: locked, blocked, never scheduled.
	tidc := make(chan int)
	stopLoop := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		tidc <- unix.Gettid()
		<-stopLoop
	}()
	loopTid := <-tidc
	defer close(stopLoop)
	snapSetLoops([]loopThread{{Tid: loopTid, From: 0, To: -1}})
	defer snapSetLoops(nil)

	const mib = 1
	want := patterned(mib << 20)
	buf := make([]byte, len(want))

	var mu sync.Mutex
	headDelay, midDelay := time.Duration(0), time.Duration(0)
	hold := false
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			cn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(cn net.Conn) {
				defer func() { _ = cn.Close() }()
				br := bufio.NewReader(cn)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					if req.URL.Path == "/ping" {
						_, _ = io.WriteString(cn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
						continue
					}
					mu.Lock()
					hd, md, h := headDelay, midDelay, hold
					mu.Unlock()
					time.Sleep(hd)
					_, _ = fmt.Fprintf(cn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/octet-stream\r\n\r\n", len(want))
					_, _ = cn.Write(want[:len(want)/2])
					if h {
						time.Sleep(time.Minute)
						return
					}
					time.Sleep(md)
					_, _ = cn.Write(want[len(want)/2:])
				}
			}(cn)
		}
	}()
	addr := ln.Addr().String()
	set := func(hd, md time.Duration, h bool) {
		mu.Lock()
		headDelay, midDelay, hold = hd, md, h
		mu.Unlock()
	}

	t.Run("healthy", func(t *testing.T) {
		set(0, 0, false)
		for i := 1; i <= 5; i++ {
			r := fetchLarge(c, addr, "/big", want, buf, 4*time.Second, i)
			if !r.OK || len(r.Snaps) != 0 {
				t.Fatalf("healthy rep %d: ok=%v snapshots=%d err=%q (a snapshot or a failure on a healthy transfer is a false positive)", i, r.OK, len(r.Snaps), r.Err)
			}
		}
	})
	t.Run("head-delay-passes-with-snapshot", func(t *testing.T) {
		set(2200*time.Millisecond, 0, false)
		r := fetchLarge(c, addr, "/big", want, buf, 4*time.Second, 1)
		checkSnap(t, r, true, 1)
		// A delay in the stub's HEAD means the client had 0 bytes.
		if !strings.Contains(r.Snaps[0].Full, "0 bytes received") {
			t.Errorf("the dump does not say 0 bytes were received:\n%s", r.Snaps[0].Full)
		}
	})
	t.Run("mid-body-delay-passes-with-snapshot", func(t *testing.T) {
		set(0, 2200*time.Millisecond, false)
		r := fetchLarge(c, addr, "/big", want, buf, 4*time.Second, 1)
		checkSnap(t, r, true, 1)
		if !strings.Contains(r.Snaps[0].Digest, "nothing queued on either socket") {
			t.Errorf("a stub that stopped writing should read as 'nothing queued':\n%s", r.Snaps[0].Digest)
		}
		if r.MaxIdleNs < int64(2*time.Second) || r.MaxIdleAt < len(want)/2 || r.MaxIdleAt > len(want)/2+512 {
			t.Errorf("longest wait %s ms began at byte %d, want >= 2000 ms at byte %d (+ the response head)", ms(r.MaxIdleNs), r.MaxIdleAt, len(want)/2)
		}
	})
	t.Run("stall-forever-fails-with-snapshot", func(t *testing.T) {
		set(0, 0, true)
		start := time.Now()
		r := fetchLarge(c, addr, "/big", want, buf, 2500*time.Millisecond, 1)
		took := time.Since(start)
		checkSnap(t, r, false, 1)
		if took < 2400*time.Millisecond || took > 3500*time.Millisecond {
			t.Errorf("the 2.5 s rule fired after %v: the snapshots must not move it", took)
		}
		if !strings.Contains(r.Err, "then no byte for 2.5s") {
			t.Errorf("failure text %q does not carry the no-byte rule", r.Err)
		}
		s := r.Snaps[0]
		if s.Recovered || !strings.Contains(s.Outcome, "NO BYTE") {
			t.Errorf("outcome %q should be the no-byte rule", s.Outcome)
		}
		// The digest is what the failure message carries.
		if !strings.Contains(s.Digest, "1 did not (frozen)") {
			t.Errorf("the blocked stand-in loop thread should be counted frozen:\n%s", s.Digest)
		}
		logf(t, "the failure message would carry:\nrep failed: %s\n%s", r.Err, s.Digest)
	})
}

func checkSnap(t *testing.T, r repResult, wantOK bool, n int) {
	t.Helper()
	if r.OK != wantOK || len(r.Snaps) != n {
		t.Fatalf("ok=%v (want %v), snapshots=%d (want %d), err=%q", r.OK, wantOK, len(r.Snaps), n, r.Err)
	}
	s := r.Snaps[0]
	for _, w := range []string{"threads", "LOOP(pin", "sockets", "client-side", "server-side", "listener", "client TCP_INFO", "per-CPU frequency", "runtime:"} {
		if !strings.Contains(s.Full, w) {
			t.Errorf("the full dump lacks %q:\n%s", w, s.Full)
		}
	}
	for _, w := range []string{"loop threads:", "sockets: server-side Send-Q", "host: PSI", "reading:"} {
		if !strings.Contains(s.Digest, w) {
			t.Errorf("the digest lacks %q:\n%s", w, s.Digest)
		}
	}
	logf(t, "%s\n%s", s.Digest, s.Full)
}
