package timeout

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

func serve888(t *testing.T, routes func(*celeris.Server)) string {
	t.Helper()
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	routes(s)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("server stopped: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not stop within 10s")
		}
	})
	return "http://" + ln.Addr().String()
}

func get888(t *testing.T, url string) (int, string) {
	t.Helper()
	cl := &http.Client{Timeout: 5 * time.Second}
	var resp *http.Response
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; {
		resp, err = cl.Get(url)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestTimeoutAfterUnusedStreamWriterRunsErrorHandler888: a handler that took
// the StreamWriter and sent nothing before it overran its deadline (or
// returned a timeout error) has not answered, but IsWritten said true, so the
// middleware returned ErrServiceUnavailable and its ErrorHandler never ran.
// The default ErrorHandler hides it (the router's 503 is the same), so the
// test uses a custom one.
func TestTimeoutAfterUnusedStreamWriterRunsErrorHandler888(t *testing.T) {
	errSlow := errors.New("slow upstream")
	base := serve888(t, func(s *celeris.Server) {
		s.Use(New(Config{
			Timeout:       30 * time.Millisecond,
			TimeoutErrors: []error{errSlow},
			ErrorHandler: func(c *celeris.Context, _ error) error {
				return c.String(418, "timed-out888")
			},
		}))
		s.GET("/overrun", func(c *celeris.Context) error {
			if c.StreamWriter() == nil {
				return celeris.NewHTTPError(500, "no StreamWriter")
			}
			time.Sleep(120 * time.Millisecond)
			return nil
		})
		s.GET("/err", func(c *celeris.Context) error {
			_ = c.StreamWriter()
			return errSlow
		})
		// Control: once the stream is on the wire the response is committed
		// and the ErrorHandler must not run.
		s.GET("/used", func(c *celeris.Context) error {
			sw := c.StreamWriter()
			if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
				return err
			}
			_, _ = sw.Write([]byte("part888"))
			time.Sleep(120 * time.Millisecond)
			return nil
		})
	})
	for _, p := range []string{"/overrun", "/err"} {
		if st, body := get888(t, base+p); st != 418 || body != "timed-out888" {
			t.Errorf("GET %s: %d %q, want 418 \"timed-out888\" (the ErrorHandler's answer)", p, st, body)
		}
	}
	if st, body := get888(t, base+"/used"); st != 200 || body == "timed-out888" {
		t.Errorf("GET /used: %d %q, want the stream and no ErrorHandler body", st, body)
	}
}

// TestNoReclaimWhenTheHandlerBeatTheDeadline888: the unused StreamWriter is
// given back only on the paths that answer a timeout. A handler that took the
// writer and returned in time leaves the context as it found it, so a
// middleware above this one still reads IsWritten as true (and the request
// pays no call for it).
func TestNoReclaimWhenTheHandlerBeatTheDeadline888(t *testing.T) {
	var sawWritten atomic.Bool
	base := serve888(t, func(s *celeris.Server) {
		s.Use(func(c *celeris.Context) error {
			err := c.Next()
			sawWritten.Store(c.IsWritten())
			return err
		})
		s.Use(New(Config{Timeout: time.Second}))
		s.GET("/quick", func(c *celeris.Context) error {
			_ = c.StreamWriter()
			return nil
		})
	})
	get888(t, base+"/quick")
	if !sawWritten.Load() {
		t.Error("IsWritten is false above the timeout after an in-time handler took the StreamWriter: the writer was reclaimed on the success path")
	}
}
