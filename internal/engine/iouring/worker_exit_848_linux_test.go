//go:build linux

package iouring

import (
	"context"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#848 at the engine: a handler that runs on the worker and calls
// runtime.Goexit ends the worker goroutine. The engine counts it
// (abnormalWorkerExits) and logs one Error. A worker that returns, by whatever
// path, is not counted. One worker everywhere: the CI step that runs this
// package at 8 MiB of memlock funds one.

// boomID848 is the id of the loop or worker the last /boom ran on.
var boomID848 atomic.Int64

type goexitHandler848 struct{}

func (goexitHandler848) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/boom" {
		id, _ := ctxkit.WorkerIDFrom(ctx)
		boomID848.Store(int64(id))
		runtime.Goexit()
	}
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

type logRec848 struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type logCap848 struct {
	mu   sync.Mutex
	recs []logRec848
}

func (c *logCap848) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCap848) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCap848) WithGroup(string) slog.Handler            { return c }
func (c *logCap848) Handle(_ context.Context, r slog.Record) error {
	rc := logRec848{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool { rc.attrs[a.Key] = a.Value.Resolve().String(); return true })
	c.mu.Lock()
	c.recs = append(c.recs, rc)
	c.mu.Unlock()
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

func start848(t *testing.T, addr string, disableDefer bool) (*Engine, *logCap848, context.CancelFunc, chan error) {
	t.Helper()
	needWorkers895(t, 1)
	logs := &logCap848{}
	var (
		e      *Engine
		cancel context.CancelFunc
		done   chan error
	)
	_, _, err := retryRingENOMEM(func() error {
		var nerr error
		e, nerr = New(resource.Config{
			Addr:               addr,
			Engine:             engine.IOUring,
			Protocol:           engine.HTTP1,
			Logger:             slog.New(logs),
			DisableDeferAccept: disableDefer,
			Resources:          resource.Resources{Workers: 2}, // capped to what memlock funds
		}, goexitHandler848{})
		if nerr != nil {
			skipOrFail656(t, "New: %v", nerr)
		}
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		done = make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
			if e.Addr() != nil {
				return nil
			}
			select {
			case lerr := <-done:
				cancel()
				return lerr
			case <-time.After(time.Millisecond):
			}
		}
		cancel()
		<-done
		t.Fatalf("engine on %s never published an address", addr)
		return nil
	})
	if err != nil {
		if ioUringUnavailable639(err) {
			skipUnlessIOUringUnusable(t, err)
		}
		t.Fatalf("engine start on %s: %v", addr, err)
	}
	t.Cleanup(func() { cancel(); <-done })
	return e, logs, cancel, done
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

func TestSyncHandlerGoexitEndsOneWorkerLoudly848(t *testing.T) {
	e, logs, cancel, done := start848(t, "127.0.0.1:0", false)
	// Several keep-alive conns, each proven served: the loops own them.
	var conns []net.Conn
	for range 6 {
		c, err := net.DialTimeout("tcp", e.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write([]byte("GET /ok HTTP/1.1\r\nHost: x\r\n\r\n"))
		buf := make([]byte, 256)
		if n, err := c.Read(buf); err != nil || !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
			t.Fatalf("PREMISE: /ok before the fault: %q %v", buf[:n], err)
		}
		conns = append(conns, c)
	}
	_, _ = conns[0].Write([]byte("GET /boom HTTP/1.1\r\nHost: x\r\n\r\n"))
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl) && e.abnormalWorkerExits.Load() == 0; time.Sleep(5 * time.Millisecond) {
	}
	time.Sleep(200 * time.Millisecond)
	stop848(t, cancel, done)
	if got := e.abnormalWorkerExits.Load(); got != 1 {
		t.Fatalf("abnormalWorkerExits = %d, want 1", got)
	}
	recs := logs.exits()
	if len(recs) != 1 {
		t.Fatalf("%d exit records, want 1", len(recs))
	}
	r := recs[0]
	if r.level != slog.LevelError || r.attrs["engine"] != "io_uring" || r.attrs["worker"] != strconv.FormatInt(boomID848.Load(), 10) {
		t.Errorf("record = level %s attrs %v, want ERROR engine=io_uring worker=%d", r.level, r.attrs, boomID848.Load())
	}
	if n, err := strconv.Atoi(r.attrs["conns"]); err != nil || n < 1 || n > len(conns) {
		t.Errorf("conns = %q, want 1..%d (the loop's share of %d conns, the boom conn among them)", r.attrs["conns"], len(conns), len(conns))
	}
}

func TestNormalWorkerReturnsAreNotCounted848(t *testing.T) {
	needWorkers895(t, 1)
	t.Run("cancel", func(t *testing.T) {
		e, logs, cancel, done := start848(t, "127.0.0.1:0", false)
		stop848(t, cancel, done)
		if e.abnormalWorkerExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("a cancel counted %d, logged %d", e.abnormalWorkerExits.Load(), len(logs.exits()))
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
			Addr: hold.Addr().String(), Engine: engine.IOUring, Protocol: engine.HTTP1,
			Logger: slog.New(logs), Resources: resource.Resources{Workers: 2},
		}, goexitHandler848{})
		if err != nil {
			skipOrFail656(t, "New: %v", err)
		}
		if err := e.Listen(context.Background()); err == nil || !strings.Contains(err.Error(), "address already in use") {
			t.Fatalf("PREMISE: Listen on a held port returned %v, want address already in use", err)
		}
		if e.abnormalWorkerExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("a setup failure counted %d, logged %d", e.abnormalWorkerExits.Load(), len(logs.exits()))
		}
	})
	t.Run("pause-resume", func(t *testing.T) {
		e, logs, cancel, done := start848(t, "127.0.0.1:0", true)
		if err := e.PauseAccept(); err != nil {
			t.Fatal(err)
		}
		if err := e.ResumeAccept(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		stop848(t, cancel, done)
		if e.abnormalWorkerExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("pause/resume counted %d, logged %d", e.abnormalWorkerExits.Load(), len(logs.exits()))
		}
	})
	t.Run("lost-listener", func(t *testing.T) {
		tmp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := tmp.Addr().String()
		_ = tmp.Close()
		e, logs, cancel, done := start848(t, addr, true)
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
		want := uint64(e.Metrics().Workers)
		var lost uint64
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl) && lost < want; time.Sleep(5 * time.Millisecond) {
			lost = e.Metrics().ErrorListenerRecreate
		}
		if want == 0 || lost != want {
			t.Fatalf("PREMISE: ErrorListenerRecreate = %d, want %d (one per worker)", lost, want)
		}
		time.Sleep(100 * time.Millisecond)
		stop848(t, cancel, done)
		if e.abnormalWorkerExits.Load() != 0 || len(logs.exits()) != 0 {
			t.Fatalf("a lost listener (the loop ends itself) counted %d, logged %d", e.abnormalWorkerExits.Load(), len(logs.exits()))
		}
	})
}
