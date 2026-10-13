//go:build linux

package epoll

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A loop that ended without returning (a sync handler called
// runtime.Goexit, celeris#848) must leave the SO_REUSEPORT group, and a pause
// must not wait on it (the Goexit item from celeris#848 on the checklist celeris#885). Before, its listener stayed open and
// took a share of new connections that nothing would accept, PauseAccept waited
// to its bound (Linger()+1s, about 2.5 s) and returned nil, and a second
// pause after a ResumeAccept waited the same again: ResumeAccept clears the
// flag the dead loop had set and nothing sets it twice.
//
// The listeners are made without TCP_DEFER_ACCEPT, so the live loops of the
// engine close theirs at the pause at once: the only wait left in a pause is
// the dead one. The test runs in a child process (child848): a loop that
// exits on a Goexit never reaches shutdown and leaves its descriptors open.
//
// It asserts behaviour only: dials, the time PauseAccept takes, and the count
// of abnormal exits.

// okOnce848 dials addr once and returns the id the server's /ok body names.
func okOnce848(addr string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("GET /ok HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return "", err
	}
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		return "", err
	}
	got := string(buf[:n])
	if !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.Contains(got, "w=") {
		return "", errors.New("not a 200 with a worker id: " + got)
	}
	return got[strings.LastIndex(got, "w="):], nil
}

func TestGoexitedLoopLeavesTheGroupAndThePauseDoesNotWaitForIt848(t *testing.T) {
	if !child848(t) {
		return
	}
	e, _, cancel, done := start848(t, "127.0.0.1:0", 4, true)
	addr := e.Addr().String()

	// The ids that serve: dial until at least two ids are seen, or give up on
	// finding a second (a single loop: the engine funds one at the CI step's
	// 8 MiB memlock), and keep the conn of the last one to take the fault.
	ids := map[string]bool{}
	var boomConn net.Conn
	for i := 0; i < 64 && (i < 12 || (len(ids) < 2 && i < 48)); i++ {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write([]byte("GET /ok HTTP/1.1\r\nHost: x\r\n\r\n"))
		buf := make([]byte, 256)
		n, err := c.Read(buf)
		if err != nil || !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
			t.Fatalf("PREMISE: /ok before the fault: %q %v", buf[:n], err)
		}
		ids[string(buf[strings.LastIndex(string(buf[:n]), "w="):n])] = true
		if boomConn != nil {
			_ = boomConn.Close()
		}
		boomConn = c
	}
	defer func() { _ = boomConn.Close() }()
	multi := len(ids) >= 2

	_, _ = boomConn.Write([]byte("GET /boom HTTP/1.1\r\nHost: x\r\n\r\n"))
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl) && e.abnormalLoopExits.Load() == 0; time.Sleep(5 * time.Millisecond) {
	}
	if got := e.abnormalLoopExits.Load(); got != 1 {
		t.Fatalf("PREMISE: %s = %d, want 1", "abnormalLoopExits", got)
	}

	// 1. Fresh dials are all answered: none lands on the dead listener.
	// Needs a loop left to answer. The wait is for the kernel to drop the
	// dead listener, which it may do a moment after the close (io_uring: when
	// the ring's teardown has run); 32 dials in a row, each answered.
	answeredAfter := time.Duration(-1)
	if multi {
		start := time.Now()
		for dl := start.Add(10 * time.Second); time.Now().Before(dl); {
			ok := 0
			for ok < 32 {
				if _, err := okOnce848(addr); err != nil {
					break
				}
				ok++
			}
			if ok == 32 {
				answeredAfter = time.Since(start)
				break
			}
		}
		if answeredAfter < 0 {
			t.Errorf("fresh dials: 32 in a row were never all answered within 10 s of the fault: the dead loop's listener still takes connections")
		}
	}

	// 2. PauseAccept does not wait for the dead loop.
	t0 := time.Now()
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	firstPause := time.Since(t0)
	if firstPause > 800*time.Millisecond {
		t.Errorf("PauseAccept took %v, want under 800 ms (a pause waited for the dead loop to its bound)", firstPause)
	}

	// 3. After the pause no dial is accepted: the dead listener is closed too,
	// and it has left the group by the time PauseAccept returns, so the very
	// first dial is refused (no polling), and the refusal holds for 20 dials.
	refusedAfter := time.Duration(-1)
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		_ = c.Close()
		t.Errorf("the first dial after PauseAccept returned was accepted, %v after the pause began: the dead listener was still in the SO_REUSEPORT group", time.Since(t0))
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a dial after the pause failed with %v, want connection refused", err)
	} else {
		refusedAfter = time.Since(t0)
		for i := 0; i < 20; i++ {
			if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
				_ = c.Close()
				t.Errorf("dial %d after the first refusal was accepted", i)
				break
			}
		}
	}

	// 4. Resume, then Pause again: the second pause does not wait either.
	if err := e.ResumeAccept(); err != nil {
		t.Fatalf("ResumeAccept: %v", err)
	}
	resumedAnswered := !multi
	if multi {
		for dl := time.Now().Add(10 * time.Second); time.Now().Before(dl) && !resumedAnswered; time.Sleep(10 * time.Millisecond) {
			ok := 0
			for ok < 16 {
				if _, err := okOnce848(addr); err != nil {
					break
				}
				ok++
			}
			resumedAnswered = ok == 16
		}
		if !resumedAnswered {
			t.Errorf("after ResumeAccept 16 fresh dials in a row were never all answered within 10 s")
		}
	}
	t1 := time.Now()
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("second PauseAccept: %v", err)
	}
	secondPause := time.Since(t1)
	if secondPause > 800*time.Millisecond {
		t.Errorf("Pause-Resume-Pause: the second PauseAccept took %v, want under 800 ms (ResumeAccept cleared the dead loop's flag and nothing set it again)", secondPause)
	}
	if err := e.ResumeAccept(); err != nil {
		t.Fatalf("ResumeAccept: %v", err)
	}
	stop848(t, cancel, done)
	t.Logf("celeris848 RESULT epoll-lifecycle ids=%d multi=%v answered_after=%v first_pause=%v refused_after=%v resumed_answered=%v second_pause=%v counted=%d",
		len(ids), multi, answeredAfter, firstPause, refusedAfter, resumedAnswered, secondPause, e.abnormalLoopExits.Load())
}
