package recovery

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// serve888 starts a std-engine server whose routes the callback registers and
// returns the base URL. The std engine answers on every platform and gives
// Context.StreamWriter a Streamer, so a StreamWriter taken and never used is
// reproducible without a linux-only engine.
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

// TestPanicAfterUnusedStreamWriterReachesErrorHandler888: a handler that took
// the StreamWriter (sse.New does, before OnConnect) and panicked before it
// sent anything has sent nothing, but Context.IsWritten said true, so recovery
// took the "panic after response committed" path: its ErrorHandler never ran
// and the panic was logged as a committed response.
func TestPanicAfterUnusedStreamWriterReachesErrorHandler888(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	base := serve888(t, func(s *celeris.Server) {
		s.Use(New(Config{
			Logger: log,
			ErrorHandler: func(c *celeris.Context, _ error) error {
				return c.String(418, "recovered888")
			},
		}))
		s.GET("/unused", func(c *celeris.Context) error {
			if c.StreamWriter() == nil {
				return celeris.NewHTTPError(500, "no StreamWriter")
			}
			panic("before the stream is used")
		})
		s.GET("/used", func(c *celeris.Context) error {
			sw := c.StreamWriter()
			if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
				return err
			}
			_, _ = sw.Write([]byte("part888"))
			panic("after the stream is used")
		})
	})
	if st, body := get888(t, base+"/unused"); st != 418 || body != "recovered888" {
		t.Errorf("panic before the StreamWriter was used: %d %q, want 418 \"recovered888\" (the ErrorHandler's answer)", st, body)
	}
	if strings.Contains(logs.String(), "response committed") && strings.Contains(logs.String(), "before the stream is used") {
		t.Errorf("a panic before the StreamWriter was used is logged as after the response was committed:\n%s", logs.String())
	}
	// The other side of the rule: once the stream is on the wire the response
	// is committed, and the ErrorHandler must not run.
	st, body := get888(t, base+"/used")
	if st != 200 || strings.Contains(body, "recovered888") {
		t.Errorf("panic after the StreamWriter was used: %d %q, want the stream (200) and no ErrorHandler body", st, body)
	}
}
