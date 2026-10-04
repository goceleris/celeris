package otel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.32.0"
)

// metricPoints returns, for each HTTP server instrument the middleware
// records, its data points' attribute sets.
func metricPoints(t *testing.T, rm metricdata.ResourceMetrics) map[string][]attribute.Set {
	t.Helper()
	out := map[string][]attribute.Set{}
	for _, name := range []string{"http.server.request.duration", "http.server.active_requests", "http.server.request.body.size", "http.server.response.body.size"} {
		m := findMetric(rm, name)
		if m == nil {
			t.Fatalf("%s not recorded", name)
		}
		switch d := m.Data.(type) {
		case metricdata.Histogram[float64]:
			for _, p := range d.DataPoints {
				out[name] = append(out[name], p.Attributes)
			}
		case metricdata.Histogram[int64]:
			for _, p := range d.DataPoints {
				out[name] = append(out[name], p.Attributes)
			}
		case metricdata.Sum[int64]:
			for _, p := range d.DataPoints {
				out[name] = append(out[name], p.Attributes)
			}
		default:
			t.Fatalf("%s: unexpected data %T", name, m.Data)
		}
	}
	return out
}

// TestMetricsDoNotKeyOnTheClientsHost924: celeris#924. server.address (the
// request's Host header, which the client chooses) was in every metric
// attribute set by default, so one route answered under n made-up Host
// values made n series per instrument: past the SDK's cardinality limit
// (2000) every later request went into the overflow series, and with no
// limit memory grew without bound. OTel's semantic conventions make
// server.address Opt-In on the HTTP server metrics for this reason. By
// default the metrics no longer carry it (the span still does), and
// MetricServerAddress opts in.
func TestMetricsDoNotKeyOnTheClientsHost924(t *testing.T) {
	const hosts = 50
	for _, tc := range []struct {
		name       string
		optIn      bool
		wantPoints int
	}{
		{"default", false, 1},
		{"MetricServerAddress", true, hosts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp, exp, mp, reader := newTestMetrics(t)
			defer func() {
				_ = tp.Shutdown(context.Background())
				_ = mp.Shutdown(context.Background())
			}()
			mw := New(Config{TracerProvider: tp, MeterProvider: mp, MetricServerAddress: tc.optIn})
			h := func(c *celeris.Context) error { return c.String(200, "ok") }
			for i := range hosts {
				// celeristest fixes :authority; the client's Host arrives as
				// c.Host() does on a server (the real-server test below
				// sends it).
				host := fmt.Sprintf("made-up-%d.example", i)
				setHost := func(c *celeris.Context) error { c.SetHost(host); return c.Next() }
				if err := runChain(t, []celeris.HandlerFunc{setHost, mw, h}, "POST", "/users",
					celeristest.WithBody([]byte("{}")), celeristest.WithHeader("content-length", "2")); err != nil {
					t.Fatal(err)
				}
			}
			for name, sets := range metricPoints(t, collectMetrics(t, reader)) {
				if len(sets) != tc.wantPoints {
					t.Errorf("%s: %d series for one route under %d Host values, want %d", name, len(sets), hosts, tc.wantPoints)
				}
				for _, s := range sets {
					if _, has := s.Value(semconv.ServerAddressKey); has != tc.optIn {
						t.Errorf("%s: server.address present=%v, want %v", name, has, tc.optIn)
						break
					}
				}
			}
			// The span keeps server.address either way.
			spans := exp.GetSpans()
			if len(spans) != hosts {
				t.Fatalf("%d spans, want %d", len(spans), hosts)
			}
			found := false
			for _, a := range spans[hosts-1].Attributes {
				if a.Key == semconv.ServerAddressKey && a.Value.AsString() == fmt.Sprintf("made-up-%d.example", hosts-1) {
					found = true
				}
			}
			if !found {
				t.Errorf("the span lost server.address")
			}
		})
	}
}

// TestMetricsKeepTheirSeriesUnderManyHostsOnARealServer924 is the issue's
// measurement on a real server (std engine): one route answered under 2100
// made-up Host values, past the SDK's default cardinality limit of 2000 per
// instrument, then once under api.example.com. Every request must land in
// the route's one series, with no overflow series.
func TestMetricsKeepTheirSeriesUnderManyHostsOnARealServer924(t *testing.T) {
	tp, _, mp, reader := newTestMetrics(t)
	defer func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	}()
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.Use(New(Config{TracerProvider: tp, MeterProvider: mp}))
	s.GET("/users", func(c *celeris.Context) error { return c.String(200, "ok") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
	defer func() { cancel(); <-done }()
	cl := &http.Client{Timeout: 5 * time.Second}
	get := func(host string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; {
			req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/users", nil)
			req.Host = host
			resp, err := cl.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	const n = 2100
	for i := range n {
		get(fmt.Sprintf("h%d.attacker.example", i))
	}
	get("api.example.com")
	m := findMetric(collectMetrics(t, reader), "http.server.request.duration")
	if m == nil {
		t.Fatal("no http.server.request.duration")
	}
	hist := m.Data.(metricdata.Histogram[float64])
	var count uint64
	for _, dp := range hist.DataPoints {
		if v, ok := dp.Attributes.Value("otel.metric.overflow"); ok && v.AsBool() {
			t.Errorf("an overflow series: %d requests in it", dp.Count)
		}
		if _, ok := dp.Attributes.Value(semconv.ServerAddressKey); ok {
			t.Errorf("a series carries server.address")
		}
		count += dp.Count
	}
	if len(hist.DataPoints) != 1 || count != n+1 {
		t.Errorf("%d series holding %d requests, want 1 series holding %d", len(hist.DataPoints), count, n+1)
	}
	t.Logf("%d requests under %d Host values: %d series", count, n+1, len(hist.DataPoints))
}
