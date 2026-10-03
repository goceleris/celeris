package metrics_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/metrics"
)

// requestsTotal852 returns requests_total as "method|path|status" -> count.
func requestsTotal852(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Errorf("Gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "celeris_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			out[l["method"]+"|"+l["path"]+"|"+l["status"]] = m.GetCounter().GetValue()
		}
	}
	return out
}

// checkUnmatchedMethodSeries852 asserts the series a run of
// sendUnmatchedMethods852-shaped traffic leaves: every request no route
// matched with a method outside the standard set in one "_OTHER" series per
// path sentinel, standard and routed methods under their own names.
func checkUnmatchedMethodSeries852(t *testing.T, got map[string]float64, want map[string]float64) {
	t.Helper()
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("requests_total{%s} = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d requests_total series, want %d (one per distinct client method would be unbounded): %.600q", len(got), len(want), keys)
	}
}

// TestUnmatchedMethodLabelBounded852 pins the metrics side of celeris#852.
// The global middleware now sees every request no route matches, and such a
// request carries whatever method the client sent: labelled verbatim, each
// distinct method would be a new series kept for the life of the registry,
// for any unauthenticated client. A request no route matched is labelled
// "_OTHER" for a method outside the standard set (as the otel middleware
// does); a standard method keeps its name, and so does a routed custom
// method (bounded by the routes).
func TestUnmatchedMethodLabelBounded852(t *testing.T) {
	const n = 40
	reg := prometheus.NewRegistry()
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.Use(metrics.New(metrics.Config{Registry: reg}))
	s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "pong") })
	s.Handle("PURGE", "/cache", func(c *celeris.Context) error { return c.String(200, "purged") })
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
	do := func(method, path string, want int) {
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
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s %s: %d, want %d", method, path, resp.StatusCode, want)
		}
	}
	do("GET", "/ping", 200)
	for i := range n {
		do(fmt.Sprintf("GZ%02d", i), "/nope", 404)
		do(fmt.Sprintf("GZ%02d", i), "/ping", 405)
	}
	do("GET", "/nope", 404)
	do("DELETE", "/ping", 405)
	do("PURGE", "/cache", 200)

	got := requestsTotal852(t, reg)
	for k := range got {
		if strings.HasPrefix(k, "GZ") {
			t.Errorf("a client's method became a label value: requests_total{%s}", k)
			break
		}
	}
	checkUnmatchedMethodSeries852(t, got, map[string]float64{
		"GET|/ping|200":                   1,
		"_OTHER|<unmatched>|404":          n,
		"_OTHER|<method-not-allowed>|405": n,
		"GET|<unmatched>|404":             1,
		"DELETE|<method-not-allowed>|405": 1,
		"PURGE|/cache|200":                1,
	})
}
