//go:build linux

package iouring

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#1242. A stale-close canary (sockets of its own, checked by fstat)
// found one close of a descriptor number its owner no longer held, in the
// Goexit exit path of a worker. The number may by then name another
// connection or a sibling worker's listener, so a real server sees a random
// connection or listener vanish.
//
// fdTrap1242 is the ownership check behind fdCloseSeam. Every close the engine
// makes goes through it, and it does not close(2): it dup3s a placeholder (the
// read end of a pipe of its own) over the number, which closes what the number
// named and keeps the number taken. Nothing can re-open a number the trap
// holds, so a later close of a number that still names the placeholder is the
// engine closing something it no longer owns, whatever else runs, and the
// verdict does not depend on timing. A close of a number that is not open at
// all is the same fault with no reuse yet. A number that no longer names the
// placeholder was released by someone else and may be opened again: its next
// close is a first one.
type fdTrap1242 struct {
	tomb      int // the read end of the placeholder pipe
	tombDev   uint64
	tombIno   uint64
	pipeSpare int // the write end, kept open so the placeholder never reads EOF
	mu        sync.Mutex
	last      map[int]string // number -> the site that closed it
	bad       []string
	n         atomic.Int64
	keep      int // when > 0, the placeholders of the oldest closes are released past this many (a whole-package run would run out of descriptor numbers)
	order     []int
	removed   bool // release ran: a late close (a worker an earlier test leaked) is a plain close(2)
	echo      bool // also write each close and each fault to stderr as it happens (a panicking worker ends the process before any verdict)
}

// newFDTrap1242 makes a trap with its placeholder; release gives back the
// descriptors it holds and the numbers it kept.
func newFDTrap1242() (*fdTrap1242, error) {
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(p[0], &st); err != nil {
		return nil, err
	}
	return &fdTrap1242{tomb: p[0], pipeSpare: p[1], tombDev: st.Dev, tombIno: st.Ino, last: map[int]string{}}, nil
}

func (tr *fdTrap1242) release() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.removed = true
	for fd := range tr.last {
		if tr.isPlaceholder(fd) {
			_ = unix.Close(fd)
		}
	}
	_ = unix.Close(tr.tomb)
	_ = unix.Close(tr.pipeSpare)
}

// installFDTrap1242 sets the seam for the test and, when the test ends, puts
// back what it replaced and releases the numbers the trap kept.
func installFDTrap1242(t *testing.T) *fdTrap1242 {
	t.Helper()
	tr, err := newFDTrap1242()
	if err != nil {
		t.Fatalf("fd trap: %v", err)
	}
	fn := tr.closeFD
	prev := fdCloseSeam.Swap(&fn)
	t.Cleanup(func() {
		fdCloseSeam.Store(prev)
		tr.release()
	})
	return tr
}

// isPlaceholder reports whether fd still names the trap's placeholder.
func (tr *fdTrap1242) isPlaceholder(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Dev == tr.tombDev && st.Ino == tr.tombIno
}

// site names the engine frames of the close, innermost first, skipping the
// seam's own.
func (tr *fdTrap1242) site() string {
	pcs := make([]uintptr, 12)
	n := runtime.Callers(4, pcs) // closeFD, closeFDErr, the seam, then the caller
	var out []string
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		name := f.Function[strings.LastIndex(f.Function, "/")+1:]
		if !strings.HasPrefix(name, "testing.") && !strings.HasPrefix(name, "runtime.") {
			out = append(out, fmt.Sprintf("%s (%s:%d)", name, f.File[strings.LastIndex(f.File, "/")+1:], f.Line))
		}
		if !more || len(out) == 4 {
			break
		}
	}
	return strings.Join(out, " <- ")
}

func (tr *fdTrap1242) fault(msg string) {
	tr.bad = append(tr.bad, msg)
	if tr.echo {
		fmt.Fprintf(os.Stderr, "celeris1242 FAULT %s\n", msg)
	}
}

func (tr *fdTrap1242) closeFD(fd int) error {
	tr.n.Add(1)
	site := tr.site()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.removed {
		// The placeholder is gone with the trap: a close that loaded it just
		// before the seam was put back closes the number, as without a trap.
		return unix.Close(fd)
	}
	if tr.echo {
		fmt.Fprintf(os.Stderr, "celeris1242 CLOSE fd=%d at %s\n", fd, site)
	}
	if first, ok := tr.last[fd]; ok && tr.isPlaceholder(fd) {
		tr.fault(fmt.Sprintf("fd %d closed a second time with nothing in between that could have re-opened it:\n\tfirst close:  %s\n\tsecond close: %s", fd, first, site))
		return unix.EBADF
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		tr.fault(fmt.Sprintf("fd %d closed while not open (%v):\n\tclose: %s", fd, err, site))
		return err
	}
	if err := unix.Dup3(tr.tomb, fd, unix.O_CLOEXEC); err != nil {
		return err
	}
	tr.last[fd] = site
	if tr.keep > 0 {
		for i, o := range tr.order { // the number was opened again since: one entry
			if o == fd {
				tr.order = append(tr.order[:i], tr.order[i+1:]...)
				break
			}
		}
		tr.order = append(tr.order, fd)
		for len(tr.order) > tr.keep {
			old := tr.order[0]
			tr.order = tr.order[1:]
			if tr.isPlaceholder(old) {
				_ = unix.Close(old)
			}
			delete(tr.last, old)
		}
	}
	return nil
}

// verdict fails the test on every fault the trap saw, naming the number and
// both sites.
func (tr *fdTrap1242) verdict(t *testing.T) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, b := range tr.bad {
		t.Errorf("celeris#1242: %s", b)
	}
}

// fdID1242 is what a descriptor number named when a test took it: a test
// that tears down a descriptor the code under test may have closed already
// asks fdID1242.names first, and closes only a number that still names the
// same file. fdOpen704 (is anything open at this number) is not that
// question: a number the code under test freed is taken by the next open in
// the process (a sibling test's engine that is still running, the Go
// runtime's own sockets), and closing it then closes that one (celeris#1242).
// Sockets have an inode each. io_uring rings do not on every kernel: use
// ringNamed1242 for one.
type fdID1242 struct{ dev, ino uint64 }

func fdIdentity1242(t testing.TB, fd int) fdID1242 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatalf("fstat(%d): %v", fd, err)
	}
	return fdID1242{st.Dev, st.Ino}
}

// names reports whether fd still names the file the identity was taken from.
func (id fdID1242) names(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Dev == id.dev && st.Ino == id.ino
}

// ringNamed1242 reports whether fd still names the io_uring ring the identity
// was taken from: the same inode and still an io_uring file, so that on a
// kernel where rings share an inode a socket that took the number is not
// taken for the ring. (Another ring on the number would be, there.)
func ringNamed1242(id fdID1242, fd int) bool {
	if !id.names(fd) {
		return false
	}
	link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	return err == nil && link == "anon_inode:[io_uring]"
}

// premiseWithin1242 is how long a premise of these tests may take to come
// true: the engine's own steps (a worker re-creating its listener after a
// resume, a handler running after its request arrived) are asynchronous and
// slow down under -race -covermode=atomic on one CPU shared with other work.
// A premise is waited for to this bound, never slept for, and one that is not
// true by then fails with what was seen.
const premiseWithin1242 = 15 * time.Second

// okWithin1242 dials until a /ok is answered, up to premiseWithin1242, and
// returns the keep-alive connection that was answered. A refused or reset
// dial is retried: a listener a worker has not re-created yet refuses.
func okWithin1242(t *testing.T, addr string) (net.Conn, time.Duration) {
	t.Helper()
	start := time.Now()
	var last error
	for time.Since(start) < premiseWithin1242 {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			_, _ = c.Write([]byte("GET /ok HTTP/1.1\r\nHost: x\r\n\r\n"))
			buf := make([]byte, 256)
			var n int
			if n, err = c.Read(buf); err == nil && strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
				return c, time.Since(start)
			} else if err == nil {
				err = fmt.Errorf("not a 200: %q", buf[:n])
			}
			_ = c.Close()
		}
		last = err
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("PREMISE: no /ok answered within %v at %s, last error: %v", premiseWithin1242, addr, last)
	return nil, 0
}

// waitFor1242 polls cond until it holds, up to premiseWithin1242.
func waitFor1242(cond func() bool) bool {
	for dl := time.Now().Add(premiseWithin1242); time.Now().Before(dl); time.Sleep(time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// stopAtCleanup1242 stops the engine when the test ends, whether or not the
// test got as far as stop848, and waits for Listen to return.
func stopAtCleanup1242(t *testing.T, cancel context.CancelFunc, done chan error) {
	t.Helper()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			done <- err
		case <-time.After(15 * time.Second):
			t.Error("Listen did not return within 15 s of the cleanup's cancel")
		}
	})
}

// Every way a worker can end closes each descriptor number it owns once. The
// canary of celeris#1242 pointed at the Goexit path, and the engine's closes
// on it are single (the stale ones were two tests' own, above); the sub-tests
// pin that for each exit path, in a child process each (child848: a worker
// that Goexits leaves descriptors open, and a sibling test's leftover engine
// would add closes of its own to the trap):
//
//	cancel                  the normal stop, with connections open
//	pause-resume            the pause's close, the re-created listener, the stop
//	goexit                  runtime.Goexit in a handler (leaveGroupAfterExit)
//	goexit-in-pause-close   runtime.Goexit in what the pause's close runs (closingFD)
//	ring-setup-failure      one worker's ring cannot be set up (releaseFailedInit)
//	initial-submit-failure  the first accept cannot be submitted (releaseFailedInit)
//	bind-failure            the listen socket cannot be bound
//	ring-wait-failure       a ring wait fails for good: the loop returns through shutdown
//	lost-listener           the resume cannot re-create the listener: the loop returns
//
// The panic path ends the process: TestPanickedWorkerClosesEachDescriptorOnce1242.
func TestEveryExitPathClosesEachDescriptorOnce1242(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		e, _, cancel, done := start848(t, "127.0.0.1:0", false)
		stopAtCleanup1242(t, cancel, done)
		// Keep-alive connections the stop has to close.
		for i := 0; i < 6; i++ {
			c, _ := okWithin1242(t, e.Addr().String())
			defer func() { _ = c.Close() }()
		}
		stop848(t, cancel, done)
		tr.requireCloses(t)
		tr.verdict(t)
	})
	t.Run("pause-resume", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		e, _, cancel, done := start848(t, "127.0.0.1:0", true)
		stopAtCleanup1242(t, cancel, done)
		for i := 0; i < 2; i++ {
			if err := e.PauseAccept(); err != nil {
				t.Fatalf("PauseAccept: %v", err)
			}
			if err := e.ResumeAccept(); err != nil {
				t.Fatalf("ResumeAccept: %v", err)
			}
		}
		// ResumeAccept returns before the workers have re-created their
		// listeners (they do it on their next iteration), so a dial right
		// after it can be refused: wait for the answer, bounded.
		c, took := okWithin1242(t, e.Addr().String())
		_ = c.Close()
		t.Logf("answered %v after the resume", took)
		stop848(t, cancel, done)
		tr.requireCloses(t)
		tr.verdict(t)
	})
	t.Run("goexit", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		e, _, cancel, done := start848(t, "127.0.0.1:0", true)
		stopAtCleanup1242(t, cancel, done)
		addr := e.Addr().String()
		for i := 0; i < 12; i++ {
			c, _ := okWithin1242(t, addr)
			_ = c.Close()
		}
		boom, _ := okWithin1242(t, addr)
		defer func() { _ = boom.Close() }()
		_, _ = boom.Write([]byte("GET /boom HTTP/1.1\r\nHost: x\r\n\r\n"))
		waitFor1242(func() bool { return e.abnormalWorkerExits.Load() != 0 })
		if got := e.abnormalWorkerExits.Load(); got != 1 {
			t.Fatalf("PREMISE: abnormalWorkerExits = %d, want 1", got)
		}
		// The lifecycle the canary saw the stale close in: two pauses and
		// resumes around the dead worker, then the stop.
		for i := 0; i < 2; i++ {
			if err := e.PauseAccept(); err != nil {
				t.Fatalf("PauseAccept: %v", err)
			}
			if err := e.ResumeAccept(); err != nil {
				t.Fatalf("ResumeAccept: %v", err)
			}
		}
		stop848(t, cancel, done)
		tr.requireCloses(t)
		tr.verdict(t)
	})
	t.Run("goexit-in-pause-close", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		e, _, cancel, done := start848(t, "127.0.0.1:0", true)
		stopAtCleanup1242(t, cancel, done)
		var fired atomic.Bool
		setCancelAcceptSeam1229(t, func(*Worker, int) {
			if fired.CompareAndSwap(false, true) {
				runtime.Goexit()
			}
		})
		if err := e.PauseAccept(); err != nil {
			t.Fatalf("PauseAccept: %v", err)
		}
		waitFor1242(func() bool { return e.abnormalWorkerExits.Load() != 0 })
		if e.abnormalWorkerExits.Load() != 1 {
			t.Fatalf("PREMISE: abnormalWorkerExits = %d, want 1", e.abnormalWorkerExits.Load())
		}
		if err := e.ResumeAccept(); err != nil {
			t.Fatalf("ResumeAccept: %v", err)
		}
		stop848(t, cancel, done)
		tr.requireCloses(t)
		tr.verdict(t)
	})
	t.Run("ring-setup-failure", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		e := newEngine656(t, freeAddr639(t), workers1242(t))
		failRingSetup656(t, func(call int32) bool { return call == 1 })
		listenExpectingInjectedFailure(t, e)
		tr.requireCloses(t)
		tr.verdict(t)
	})
	t.Run("initial-submit-failure", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		e := newEngine656(t, freeAddr639(t), workers1242(t))
		orig := submitInitialAccept
		submitInitialAccept = func(*Ring) (int, error) { return 0, errInjected656 }
		t.Cleanup(func() { submitInitialAccept = orig })
		listenExpectingInjectedFailure(t, e)
		tr.requireCloses(t)
		tr.verdict(t)
	})
	t.Run("bind-failure", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		hold, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Close() }()
		e := newEngine656(t, hold.Addr().String(), workers1242(t))
		if err := e.Listen(t.Context()); err == nil || !strings.Contains(err.Error(), "address already in use") {
			t.Fatalf("PREMISE: Listen on a held port returned %v, want address already in use", err)
		}
		tr.requireCloses(t)
		tr.verdict(t)
	})
	t.Run("ring-wait-failure", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		var failed atomic.Pointer[Worker]
		setRingWaitSeam884(t, func(w *Worker, _ ringWaitMode, err error) error {
			if err == nil && failed.CompareAndSwap(nil, w) {
				// Not EBUSY or EAGAIN, which the loop reaps and retries.
				return fmt.Errorf("io_uring_enter submit+wait: %w", unix.EIO)
			}
			return err
		})
		e, logs, cancel, done := start848(t, "127.0.0.1:0", false)
		stopAtCleanup1242(t, cancel, done)
		waitFor1242(func() bool { w := failed.Load(); return w != nil && w.exited.Load() })
		if w := failed.Load(); w == nil || !w.exited.Load() {
			t.Fatal("PREMISE: no worker left its loop after a failed ring wait")
		}
		if e.abnormalWorkerExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("PREMISE: a failed ring wait is a return, not a Goexit: counted %d, logged %d", e.abnormalWorkerExits.Load(), len(logs.exits()))
		}
		stop848(t, cancel, done)
		tr.requireCloses(t)
		tr.verdict(t)
	})
	t.Run("lost-listener", func(t *testing.T) {
		if !child848(t) {
			return
		}
		tr := installFDTrap1242(t)
		addr := freeAddr639(t)
		e, _, cancel, done := start848(t, addr, true)
		stopAtCleanup1242(t, cancel, done)
		if err := e.PauseAccept(); err != nil {
			t.Fatal(err)
		}
		// The pause's close takes the workers' listeners out of the group
		// asynchronously: bind the port once they are gone.
		var ln net.Listener
		var lerr error
		if !waitFor1242(func() bool { ln, lerr = net.Listen("tcp", addr); return lerr == nil }) {
			t.Fatalf("PREMISE: the port was never free to hold after the pause: %v", lerr)
		}
		defer func() { _ = ln.Close() }()
		if err := e.ResumeAccept(); err != nil {
			t.Fatal(err)
		}
		want := uint64(e.Metrics().Workers)
		var lost uint64
		waitFor1242(func() bool { lost = e.Metrics().ErrorListenerRecreate; return lost >= want })
		if want == 0 || lost != want {
			t.Fatalf("PREMISE: ErrorListenerRecreate = %d, want %d (one per worker)", lost, want)
		}
		stop848(t, cancel, done)
		tr.requireCloses(t)
		tr.verdict(t)
	})
}

// workers1242 is two workers, so that one can fail and its sibling be
// cancelled, or the default count (capped to one) where memlock funds no more
// (the unit job's 8 MiB): a configured count under two is refused.
func workers1242(t *testing.T) int {
	t.Helper()
	needWorkers895(t, 1)
	if maxW := MaxWorkersForMemlock(); maxW >= 0 && maxW < 2 {
		return 0
	}
	return 2
}

// requireCloses fails the test when the engine closed nothing through the
// seam: a green verdict of a trap that saw nothing would prove nothing.
func (tr *fdTrap1242) requireCloses(t *testing.T) {
	t.Helper()
	if tr.n.Load() == 0 {
		t.Fatal("PREMISE: the engine closed nothing through fdCloseSeam")
	}
}

// A panic that escapes a handler on the worker goroutine ends the process,
// after the deferred report has run leaveGroupAfterExit, so the verdict is the
// child's own output: the trap writes each close and each fault to stderr as
// it happens, and the parent reads them. Same shape as
// TestSyncWorkerPanicIsRecordedBeforeTheCrash848.
func TestPanickedWorkerClosesEachDescriptorOnce1242(t *testing.T) {
	const childEnv = "CELERIS_1242_PANIC_CHILD"
	if os.Getenv(childEnv) == "1" {
		tr := installFDTrap1242(t)
		tr.echo = true
		e, _, _, _ := start848(t, "127.0.0.1:0", false)
		c, _ := okWithin1242(t, e.Addr().String())
		_, _ = c.Write([]byte("GET /panic HTTP/1.1\r\nHost: x\r\n\r\n"))
		time.Sleep(30 * time.Second) // the panic ends the process long before
		t.Log("celeris1242 child survived the panic")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestPanickedWorkerClosesEachDescriptorOnce1242$", "-test.v", "-test.count=1", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	text := string(out)
	if strings.Contains(text, "--- SKIP") {
		t.Skipf("skipped in the child process:\n%s", text)
	}
	if err == nil || !strings.Contains(text, "panic: celeris848 panic in a handler") {
		t.Fatalf("PREMISE: the child did not die of the panic (err %v):\n%.3000s", err, text)
	}
	// The listener and the ring are the descriptors leaveGroupAfterExit closes.
	if closes := strings.Count(text, "celeris1242 CLOSE fd="); closes < 2 {
		t.Fatalf("PREMISE: %d closes seen before the crash, want at least the listener and the ring:\n%.3000s", closes, text)
	}
	for rest := text; ; {
		i := strings.Index(rest, "celeris1242 FAULT ")
		if i < 0 {
			break
		}
		rest = rest[i+len("celeris1242 FAULT "):]
		t.Errorf("celeris#1242: %.400s", rest)
	}
}

// faultsFor returns the faults reported for descriptor number fd.
func (tr *fdTrap1242) faultsFor(fd int) []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []string
	for _, b := range tr.bad {
		if strings.HasPrefix(b, fmt.Sprintf("fd %d ", fd)) {
			out = append(out, b)
		}
	}
	return out
}

// The trap itself: a second close of a number nothing re-opened is a fault
// naming the number and both sites; a number that was released and opened
// again is closed once more without one; a close of a number that is not
// open is a fault too.
func TestFDTrapFlagsOnlyAStaleClose1242(t *testing.T) {
	tr := installFDTrap1242(t)
	sock := func() int {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}

	fd := sock()
	closeFD(fd)
	if f := tr.faultsFor(fd); len(f) != 0 {
		t.Fatalf("a first close is a fault: %v", f)
	}
	closeFD(fd)
	if f := tr.faultsFor(fd); len(f) != 1 || !strings.Contains(f[0], "closed a second time") ||
		strings.Count(f[0], "TestFDTrapFlagsOnlyAStaleClose1242") != 2 {
		t.Fatalf("a second close was not reported with the number and both sites: %v", f)
	}
	tr.mu.Lock()
	tr.bad = nil // this test's verdict is the checks here
	tr.mu.Unlock()

	// Released by someone else, and taken again: the next close is a first one.
	fd = sock()
	closeFD(fd)
	if err := unix.Close(fd); err != nil { // the placeholder, closed by its owner
		t.Fatal(err)
	}
	got := sock()
	closeFD(got)
	if got == fd && len(tr.faultsFor(fd)) != 0 {
		t.Fatalf("a close of a number that was released and opened again is a fault: %v", tr.faultsFor(fd))
	}

	const never = 1 << 20
	closeFD(never)
	if f := tr.faultsFor(never); len(f) != 1 || !strings.Contains(f[0], "closed while not open") {
		t.Fatalf("a close of a number that is not open was not reported: %v", f)
	}
	tr.mu.Lock()
	tr.bad = nil
	tr.mu.Unlock()
}

// A trap that has been removed is out of the way: a worker an earlier test
// leaked may still call the function it loaded before the seam was put back,
// and its close must be a plain close(2), not a dup3 of a placeholder that is
// gone onto a number that may be someone else's by now.
func TestARemovedFDTrapClosesPlainly1242(t *testing.T) {
	tr, err := newFDTrap1242()
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	tr.release()
	if err := tr.closeFD(fd); err != nil {
		t.Fatalf("a close after the removal failed: %v", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
		_ = unix.Close(fd)
		t.Fatal("a close after the removal left the descriptor open (a placeholder was put on it)")
	}
	if f := tr.faultsFor(fd); len(f) != 0 {
		t.Fatalf("a removed trap reported: %v", f)
	}
}
