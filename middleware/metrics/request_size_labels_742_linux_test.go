//go:build linux

package metrics_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/metrics"
)

// TestRequestSizeLabelsSurviveNextRequest covers the one series of the
// celeris#732 metrics site that TestLabelValuesSurviveNextRequest cannot
// reach (celeris#742 item 6): request_size_bytes, which the middleware
// resolves on first use for a label combination whose request carries a
// body. Its label values must be the copies the middleware owns, like the
// other three metrics': on epoll and io_uring (and Adaptive, which runs
// them) the method (for one the H1 parser does not intern) and a LabelFuncs
// value read from a header are views of the connection's receive buffer,
// which the engine reuses for the connection's next request. Three requests
// with bodies, the same layout and different values go over one keep-alive
// connection; each must leave a request_size_bytes series with its own
// labels, and Gather must report no error.
func TestRequestSizeLabelsSurviveNextRequest(t *testing.T) {
	methods := []string{"TRACE", "PURGE", "MKCOL"}
	for _, a := range mwArms(t) {
		t.Run(a.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.Use(metrics.New(metrics.Config{
					Registry:   reg,
					LabelFuncs: map[string]func(*celeris.Context) string{"tenant": func(c *celeris.Context) string { return c.Header("x-tenant") }},
				}))
				for _, m := range methods {
					srv.Handle(m, "/m", func(c *celeris.Context) error { return c.String(200, "ok") })
				}
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			var want []string
			for i, m := range methods {
				v := strings.Repeat(string(rune('a'+i)), 4)
				tenant := "tenant-" + v
				keptRoundTrip(t, conn, br, m+" /m HTTP/1.1\r\nHost: x\r\nX-Tenant: "+tenant+"\r\nContent-Length: 9\r\n\r\nbody-"+v)
				want = append(want, m+"|"+tenant)
			}
			sort.Strings(want)
			got, gerr := seriesOf(reg, "celeris_request_size_bytes")
			t.Logf("MW742METRICSREQSIZE arm=%s series=%q gather_err=%v", a.name, got, gerr)
			if gerr != nil || strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("request_size_bytes series (method|tenant) %q, Gather error %v; want %q and no error", got, gerr, want)
			}
		})
	}
}

// mwArms is keptArms plus Adaptive with sync and async handlers.
func mwArms(t *testing.T) []keptArm {
	t.Helper()
	return append(keptArms(t), keptArm{"adaptive", celeris.Adaptive, false}, keptArm{"adaptive-async", celeris.Adaptive, true})
}
