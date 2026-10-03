//go:build linux

package otel_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/goceleris/celeris"
	celotel "github.com/goceleris/celeris/middleware/otel"
)

// TestCustomSliceMapAttributesSurviveNextRequest pins the rest of the otel
// site of celeris#732 (celeris#742 item 2).
//
// CustomAttributes and CustomMetricAttributes typically read request
// headers, which on epoll and io_uring (and Adaptive, which runs them) are
// views of the connection's receive buffer. The span processor keeps an
// ended span until it exports it, and the metric SDK keeps every attribute
// set as an aggregation key for the life of the provider. #736 copied STRING
// and STRINGSLICE values only. Here the request strings sit where it did
// not look: inside a SLICE value (nested one level further in a second
// SLICE), as a MAP value and a MAP key, and as an attribute key; and in a
// STRINGSLICE and a STRING, which the rewritten copy handles too. Three
// requests with the same layout and different values go over one keep-alive
// connection; every span and every duration series must keep its own
// request's attributes.
func TestCustomSliceMapAttributesSurviveNextRequest(t *testing.T) {
	spanAttrs := func(get func(string) string) []attribute.KeyValue {
		return []attribute.KeyValue{
			attribute.Slice("tags", attribute.StringValue(get("x-a")),
				attribute.SliceValue(attribute.StringValue(get("x-b")), attribute.IntValue(7))),
			attribute.Map("labels", attribute.String("v", get("x-v")), attribute.String(get("x-k"), "const")),
			attribute.Key(get("x-key")).String("by-key"),
			attribute.StringSlice("list", []string{get("x-b"), "const"}),
			attribute.String("plain", get("x-a")),
		}
	}
	metricAttrs := func(get func(string) string) []attribute.KeyValue {
		return []attribute.KeyValue{
			attribute.Map("labels", attribute.String(get("x-k"), get("x-v"))),
			attribute.Key(get("x-key")).Slice(attribute.StringValue(get("x-a"))),
		}
	}
	// emit renders the custom attributes the way the test compares them: one
	// "key=value" per attribute, the value as the SDK would export it.
	emit := func(kvs []attribute.KeyValue) string {
		var parts []string
		for _, kv := range kvs {
			parts = append(parts, string(kv.Key)+"="+kv.Value.String())
		}
		sort.Strings(parts)
		return strings.Join(parts, " ")
	}
	isCustom := func(k attribute.Key) bool {
		return k == "tags" || k == "labels" || k == "list" || k == "plain" || strings.HasPrefix(string(k), "key-")
	}

	for _, a := range mwArms(t) {
		t.Run(a.name, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.Use(celotel.New(celotel.Config{
					TracerProvider:         tp,
					MeterProvider:          mp,
					CustomAttributes:       func(c *celeris.Context) []attribute.KeyValue { return spanAttrs(c.Header) },
					CustomMetricAttributes: func(c *celeris.Context) []attribute.KeyValue { return metricAttrs(c.Header) },
				}))
				srv.GET("/o/:id", func(c *celeris.Context) error { return c.String(200, "ok") })
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			var wantSpans, wantSeries []string
			for i := range 3 {
				v := strings.Repeat(string(rune('a'+i)), 4)
				hdr := map[string]string{"x-a": "a-" + v, "x-b": "b-" + v, "x-v": "v-" + v, "x-k": "k-" + v, "x-key": "key-" + v}
				keptRoundTrip(t, conn, br, "GET /o/"+v+" HTTP/1.1\r\nHost: h\r\nX-A: "+hdr["x-a"]+"\r\nX-B: "+hdr["x-b"]+
					"\r\nX-V: "+hdr["x-v"]+"\r\nX-K: "+hdr["x-k"]+"\r\nX-Key: "+hdr["x-key"]+"\r\n\r\n")
				get := func(k string) string { return hdr[k] }
				wantSpans = append(wantSpans, emit(spanAttrs(get)))
				wantSeries = append(wantSeries, emit(metricAttrs(get)))
			}
			sort.Strings(wantSeries)

			// The middleware ends a span after the response reaches the client.
			spans := exp.GetSpans()
			for i := 0; len(spans) < 3 && i < 200; i++ {
				time.Sleep(10 * time.Millisecond)
				spans = exp.GetSpans()
			}
			if len(spans) != 3 {
				t.Fatalf("got %d spans, want 3", len(spans))
			}
			sort.Slice(spans, func(i, j int) bool { return spans[i].StartTime.Before(spans[j].StartTime) })
			var wrong []string
			for i, s := range spans {
				var custom []attribute.KeyValue
				for _, kv := range s.Attributes {
					if isCustom(kv.Key) {
						custom = append(custom, kv)
					}
				}
				if got := emit(custom); got != wantSpans[i] {
					wrong = append(wrong, fmt.Sprintf("span %d custom attributes %q (want %q)", i+1, got, wantSpans[i]))
				}
			}

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatal(err)
			}
			var series []string
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					h, ok := m.Data.(metricdata.Histogram[float64])
					if m.Name != "http.server.request.duration" || !ok {
						continue
					}
					for _, dp := range h.DataPoints {
						var custom []attribute.KeyValue
						for _, kv := range dp.Attributes.ToSlice() {
							if isCustom(kv.Key) {
								custom = append(custom, kv)
							}
						}
						series = append(series, emit(custom))
					}
				}
			}
			sort.Strings(series)
			if strings.Join(series, ",") != strings.Join(wantSeries, ",") {
				wrong = append(wrong, fmt.Sprintf("duration series custom attributes %q (want %q)", series, wantSeries))
			}
			t.Logf("MW742OTEL arm=%s spans=%d series=%d wrong=%d", a.name, len(spans), len(series), len(wrong))
			if len(wrong) > 0 {
				t.Errorf("SLICE/MAP attributes and keys kept from a request read other bytes after the connection's later requests:\n  %s", strings.Join(wrong, "\n  "))
			}
		})
	}
}

// mwArms is keptArms plus Adaptive with sync and async handlers.
func mwArms(t *testing.T) []keptArm {
	t.Helper()
	return append(keptArms(t), keptArm{"adaptive", celeris.Adaptive, false}, keptArm{"adaptive-async", celeris.Adaptive, true})
}
