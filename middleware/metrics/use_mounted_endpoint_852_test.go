package metrics_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/metrics"
)

// TestUseMountedEndpointAndUnmatchedRequests852 pins celeris#852 for this
// middleware, mounted with Server.Use as its doc says: the metrics endpoint
// answers with no route registered for it, and a request no route matches is
// counted like any other (status 404 or 405, path <unmatched> or
// <method-not-allowed>), as celeris#156 meant.
func TestUseMountedEndpointAndUnmatchedRequests852(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.Use(metrics.New(metrics.Config{Registry: reg}))
	s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "pong") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop within 10s")
		}
	}()
	base := "http://" + ln.Addr().String()
	cl := &http.Client{Timeout: 5 * time.Second}
	do := func(method, path string) (int, string) {
		t.Helper()
		var resp *http.Response
		for deadline := time.Now().Add(5 * time.Second); ; {
			req, _ := http.NewRequest(method, base+path, nil)
			resp, err = cl.Do(req)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if st, _ := do("GET", "/ping"); st != 200 {
		t.Fatalf("GET /ping: %d", st)
	}
	if st, _ := do("GET", "/nope"); st != 404 {
		t.Errorf("GET /nope: %d, want 404", st)
	}
	if st, _ := do("DELETE", "/ping"); st != 405 {
		t.Errorf("DELETE /ping: %d, want 405", st)
	}
	st, body := do("GET", "/metrics")
	if st != 200 {
		t.Fatalf("GET /metrics: %d, want 200 (the endpoint of a Use-mounted metrics middleware): %.80q", st, body)
	}
	for _, want := range []string{
		`celeris_requests_total{method="GET",path="/ping",status="200"} 1`,
		`celeris_requests_total{method="GET",path="<unmatched>",status="404"} 1`,
		`celeris_requests_total{method="DELETE",path="<method-not-allowed>",status="405"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
}
