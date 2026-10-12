//go:build linux

package epoll

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/ctxkit"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#848 at the engine: a handler that runs on the loop and calls
// runtime.Goexit ends the loop goroutine. The engine counts it
// (abnormalLoopExits) and logs one Error. A loop that returns, by whatever
// path, is not counted.

// boomID848 is the id of the loop or worker the last /boom ran on.
var boomID848 atomic.Int64

type goexitHandler848 struct{}

func (goexitHandler848) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/boom" {
		id, _ := ctxkit.WorkerIDFrom(ctx)
		boomID848.Store(int64(id))
		runtime.Goexit()
	}
	if s.Path == "/panic" {
		panic("celeris848 panic in a handler on the loop goroutine")
	}
	if s.ResponseWriter == nil {
		return nil
	}
	// The body names the loop or worker that serves the conn.
	id, _ := ctxkit.WorkerIDFrom(ctx)
	body := "w=" + strconv.Itoa(id)
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, []byte(body))
}

type logRec848 struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type logCap848 struct {
	mu   sync.Mutex
	recs []logRec848
	echo bool // also write each record to stderr (the panic test judges the order in the process's output)
}

func (c *logCap848) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCap848) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCap848) WithGroup(string) slog.Handler            { return c }
func (c *logCap848) Handle(ctx context.Context, r slog.Record) error {
	rc := logRec848{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool { rc.attrs[a.Key] = a.Value.Resolve().String(); return true })
	c.mu.Lock()
	c.recs = append(c.recs, rc)
	c.mu.Unlock()
	if c.echo {
		_ = slog.NewTextHandler(os.Stderr, nil).Handle(ctx, r)
	}
	return nil
}

func (c *logCap848) exits() []logRec848 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []logRec848
	for _, r := range c.recs {
		if strings.Contains(r.msg, "goroutine ended without returning") {
			out = append(out, r)
		}
	}
	return out
}

func start848(t *testing.T, addr string, loops int, disableDefer bool) (*Engine, *logCap848, context.CancelFunc, chan error) {
	t.Helper()
	logs := &logCap848{echo: os.Getenv(panicChildEnv848) == "1"}
	e, err := New(resource.Config{
		Addr:               addr,
		Engine:             celerisengine.Epoll,
		Protocol:           celerisengine.HTTP1,
		Logger:             slog.New(logs),
		DisableDeferAccept: disableDefer,
		Resources:          resource.Resources{Workers: loops},
	}, goexitHandler848{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(time.Millisecond) {
		if e.Addr() != nil {
			return e, logs, cancel, done
		}
	}
	t.Fatalf("engine on %s never published an address", addr)
	return nil, nil, nil, nil
}

func stop848(t *testing.T, cancel context.CancelFunc, done chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Listen did not return within 15 s of cancel")
	}
}

func TestSyncHandlerGoexitEndsOneLoopLoudly848(t *testing.T) {
	if !child848(t) {
		return
	}
	e, logs, cancel, done := start848(t, "127.0.0.1:0", 2, false)
	// Keep-alive conns, each proven served by /ok, whose body names the loop
	// that owns it. At least 6 are opened, and more until one sits on a loop
	// other than 0: a fault on loop 0 cannot tell an id that is reported right
	// from one hard-wired to 0. That conn takes the fault. If the engine runs a
	// single loop (io_uring at the CI step's 8 MiB memlock), none exists and
	// the id check proves nothing; the result line says so.
	var (
		conns []net.Conn
		ids   []string
		boom  = -1
	)
	for i := 0; i < 128 && moreConns848(i, boom, ids); i++ {
		c, err := net.DialTimeout("tcp", e.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write([]byte("GET /ok HTTP/1.1\r\nHost: x\r\n\r\n"))
		buf := make([]byte, 256)
		n, err := c.Read(buf)
		if err != nil || !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
			t.Fatalf("PREMISE: /ok before the fault: %q %v", buf[:n], err)
		}
		id := string(buf[strings.LastIndex(string(buf[:n]), "w="):n])
		if boom < 0 && id != "w=0" {
			boom = len(conns)
		}
		conns = append(conns, c)
		ids = append(ids, id)
	}
	vacuous := boom < 0
	if vacuous {
		t.Fatalf("PREMISE: %d conns on two loops all landed on loop 0 (ids %v)", len(ids), ids)
	}
	_, _ = conns[boom].Write([]byte("GET /boom HTTP/1.1\r\nHost: x\r\n\r\n"))
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl) && e.abnormalLoopExits.Load() == 0; time.Sleep(5 * time.Millisecond) {
	}
	time.Sleep(200 * time.Millisecond)
	// Only one loop died: a conn on another loop is still served.
	var otherAnswered, others int
	for k, c := range conns {
		if ids[k] == ids[boom] {
			continue
		}
		others++
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 256)
		if _, err := c.Write([]byte("GET /ok HTTP/1.1\r\nHost: x\r\n\r\n")); err == nil {
			if n, err := c.Read(buf); err == nil && strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
				otherAnswered++
			}
		}
	}
	stop848(t, cancel, done)
	t.Logf("celeris848 RESULT engine-epoll loop=%s id_check_vacuous=%v other_loops_answered=%d/%d counted=%d", strings.TrimPrefix(ids[boom], "w="), vacuous, otherAnswered, others, e.abnormalLoopExits.Load())
	if !vacuous && others == 0 {
		t.Errorf("PREMISE: no conn on another loop than %s", ids[boom])
	}
	if otherAnswered != others {
		t.Errorf("only %d of %d conns on the other loops answered after the fault: more than one loop died", otherAnswered, others)
	}
	if got := e.abnormalLoopExits.Load(); got != 1 {
		t.Fatalf("abnormalLoopExits = %d, want 1", got)
	}
	recs := logs.exits()
	if len(recs) != 1 {
		t.Fatalf("%d exit records, want 1", len(recs))
	}
	r := recs[0]
	if r.level != slog.LevelError || r.attrs["engine"] != "epoll" || r.attrs["loop"] != strings.TrimPrefix(ids[boom], "w=") || r.attrs["loop"] != strconv.FormatInt(boomID848.Load(), 10) {
		t.Errorf("record = level %s attrs %v, want ERROR engine=epoll loop=%d", r.level, r.attrs, boomID848.Load())
	}
	if n, err := strconv.Atoi(r.attrs["conns"]); err != nil || n < 1 || n > len(conns) {
		t.Errorf("conns = %q, want 1..%d (the loop's share of %d conns, the boom conn among them)", r.attrs["conns"], len(conns), len(conns))
	}
	// The stack names the call that ended the goroutine, so an operator can find the handler.
	for _, w := range []string{"runtime.Goexit", "goexitHandler848"} {
		if !strings.Contains(r.attrs["stack"], w) {
			t.Errorf("stack attr lacks %q:\n%s", w, r.attrs["stack"])
		}
	}
}

func TestNormalLoopReturnsAreNotCounted848(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		e, logs, cancel, done := start848(t, "127.0.0.1:0", 2, false)
		stop848(t, cancel, done)
		if e.abnormalLoopExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("a cancel counted %d, logged %d", e.abnormalLoopExits.Load(), len(logs.exits()))
		}
	})
	t.Run("setup-failure", func(t *testing.T) {
		hold, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Close() }()
		logs := &logCap848{}
		e, err := New(resource.Config{
			Addr: hold.Addr().String(), Engine: celerisengine.Epoll, Protocol: celerisengine.HTTP1,
			Logger: slog.New(logs), Resources: resource.Resources{Workers: 2},
		}, goexitHandler848{})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Listen(context.Background()); err == nil {
			t.Fatal("PREMISE: Listen succeeded on a held port")
		}
		if e.abnormalLoopExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("a setup failure counted %d, logged %d", e.abnormalLoopExits.Load(), len(logs.exits()))
		}
	})
	t.Run("pause-resume", func(t *testing.T) {
		e, logs, cancel, done := start848(t, "127.0.0.1:0", 2, true)
		if err := e.PauseAccept(); err != nil {
			t.Fatal(err)
		}
		if err := e.ResumeAccept(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		stop848(t, cancel, done)
		if e.abnormalLoopExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("pause/resume counted %d, logged %d", e.abnormalLoopExits.Load(), len(logs.exits()))
		}
	})
	t.Run("lost-listener", func(t *testing.T) {
		tmp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := tmp.Addr().String()
		_ = tmp.Close()
		e, logs, cancel, done := start848(t, addr, 2, true)
		if err := e.PauseAccept(); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("a fixed port has no hold: %v", err)
		}
		defer func() { _ = ln.Close() }()
		if err := e.ResumeAccept(); err != nil {
			t.Fatal(err)
		}
		var lost uint64
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl) && lost < 2; time.Sleep(5 * time.Millisecond) {
			lost = e.Metrics().ErrorListenerRecreate
		}
		if lost != 2 {
			t.Fatalf("PREMISE: ErrorListenerRecreate = %d, want 2", lost)
		}
		time.Sleep(100 * time.Millisecond)
		stop848(t, cancel, done)
		if e.abnormalLoopExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("a lost listener (the loop ends itself) counted %d, logged %d", e.abnormalLoopExits.Load(), len(logs.exits()))
		}
	})
}

const panicChildEnv848 = "CELERIS_848_PANIC_CHILD"

// A panic that escapes a handler on the loop goroutine (an engine-level
// handler: through the celeris router a panic is a 500 first) is not
// recovered, so it ends the process. The deferred report still runs while the
// panic unwinds, so the #848 record is written before the runtime prints the
// panic and exits. The child runs the engine and takes the panic; the parent
// judges the child's output (exit status, and the record before the panic).
func TestSyncLoopPanicIsRecordedBeforeTheCrash848(t *testing.T) {
	if os.Getenv(panicChildEnv848) == "1" {
		e, _, _, _ := start848(t, "127.0.0.1:0", 2, false)
		c, err := net.DialTimeout("tcp", e.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("GET /panic HTTP/1.1\r\nHost: x\r\n\r\n"))
		time.Sleep(5 * time.Second)
		t.Log("celeris848 child survived the panic")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestSyncLoopPanicIsRecordedBeforeTheCrash848$", "-test.v", "-test.count=1", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), panicChildEnv848+"=1")
	out, err := cmd.CombinedOutput()
	text := string(out)
	if strings.Contains(text, "--- SKIP") {
		t.Skipf("skipped in the child process:\n%s", text)
	}
	rec := strings.Index(text, "ended without returning")
	pan := strings.Index(text, "panic: celeris848 panic in a handler")
	t.Logf("celeris848 RESULT engine-epoll panic child_err=%v record_at=%d panic_at=%d survived=%v", err, rec, pan, strings.Contains(text, "child survived"))
	if err == nil || pan < 0 {
		t.Fatalf("PREMISE: the child did not die of the panic (err %v):\n%.3000s", err, text)
	}
	if rec < 0 || rec > pan {
		t.Fatalf("celeris#848: no loop-exit record before the panic message (record at %d, panic at %d):\n%.3000s", rec, pan, text)
	}
}

// moreConns848 reports whether the dial loop wants conn number i: at least 6,
// then until one conn sits on an id other than 0 (boom) and conns sit on more
// than one id (the survivors can be checked), and not past 24 conns that all
// share one id (a single loop or worker).
func moreConns848(i, boom int, ids []string) bool {
	if i < 6 {
		return true
	}
	same := allSame848(ids)
	if same && i >= 24 {
		return false
	}
	return boom < 0 || same
}

func allSame848(ids []string) bool {
	for _, id := range ids {
		if id != ids[0] {
			return false
		}
	}
	return true
}

// child848 runs a test that kills an event loop in a process of its own. A
// loop that exits on a Goexit never reaches shutdown, so its ring (io_uring
// ring memory is charged to RLIMIT_MEMLOCK per UID), its descriptors and its
// listen socket stay open until the process exits; left in the test binary
// they would starve every later test that starts an io_uring engine. The
// parent re-executes the test binary on this one test, in child mode, and
// judges the child's output; the child (CELERIS_848_CHILD=1) runs the body.
// It reports whether the caller is the child, which then runs the body.
func child848(t *testing.T) bool {
	t.Helper()
	if os.Getenv("CELERIS_848_CHILD") == "1" {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	parts := strings.Split(t.Name(), "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	cmd := exec.Command(exe, "-test.run="+strings.Join(parts, "/"), "-test.v", "-test.count=1", "-test.timeout=180s")
	cmd.Env = append(os.Environ(), "CELERIS_848_CHILD=1")
	out, err := cmd.CombinedOutput()
	for _, ln := range strings.Split(string(out), "\n") {
		if strings.Contains(ln, "celeris848 RESULT") {
			t.Log(strings.TrimSpace(ln))
		}
	}
	text := string(out)
	switch {
	case strings.Contains(text, "--- SKIP: "+t.Name()+" ("):
		t.Skipf("skipped in the child process:\n%s", text)
	case err != nil:
		t.Fatalf("the child process failed: %v\n%s", err, text)
	case !strings.Contains(text, "--- PASS: "+t.Name()+" ("):
		t.Fatalf("the child process did not report --- PASS for %s (not a pass):\n%s", t.Name(), text)
	case strings.Contains(text, "--- SKIP") || strings.Contains(text, "--- FAIL"):
		t.Fatalf("the child process reported a skipped subtest or a failure:\n%s", text)
	}
	return false
}
