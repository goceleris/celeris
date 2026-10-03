//go:build linux

package metrics_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/metrics"
)

// syncBuffer852 is a log sink the server's goroutines can share.
type syncBuffer852 struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer852) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer852) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// TestUnmatchedMethodLabelNativeEngines852 is TestUnmatchedMethodLabelBounded852
// on epoll and io_uring, whose H1 parser passes a method net/http refuses: one
// that is not valid UTF-8, on which client_golang's WithLabelValues panics
// (recovered by the server, with a logged stack, per request). The method
// starts with G so protocol detection keeps the connection. Every request no
// route matched lands in the "_OTHER" series, nothing panics, and Gather
// succeeds.
func TestUnmatchedMethodLabelNativeEngines852(t *testing.T) {
	const n = 20
	for _, a := range keptArms(t) {
		if a.engine == celeris.Std {
			continue // net/http answers a method that is not a token with 400
		}
		t.Run(a.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			logs := &syncBuffer852{}
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async,
					Logger: slog.New(slog.NewTextHandler(logs, nil))})
				srv.Use(metrics.New(metrics.Config{Registry: reg}))
				srv.GET("/ping", func(c *celeris.Context) error { return c.String(200, "pong") })
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			send := func(method, path string, want int) {
				t.Helper()
				if _, err := conn.Write([]byte(method + " " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("%q %s: %v", method, path, err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != want {
					t.Errorf("%q %s: %d, want %d", method, path, resp.StatusCode, want)
				}
			}
			send("G\xff\xfe", "/nope", 404)
			send("G\xff\xfe", "/ping", 405)
			for i := range n {
				send(fmt.Sprintf("GZ%02d", i), "/nope", 404)
			}

			if p := strings.Count(logs.String(), "handler panic recovered"); p != 0 {
				t.Errorf("%d recovered panics logged: a label value was not valid UTF-8", p)
			}
			checkUnmatchedMethodSeries852(t, requestsTotal852(t, reg), map[string]float64{
				"_OTHER|<unmatched>|404":          n + 1,
				"_OTHER|<method-not-allowed>|405": 1,
			})
		})
	}
}
