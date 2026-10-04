//go:build linux

package otel_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
	celotel "github.com/goceleris/celeris/middleware/otel"
)

// TestH2SchemeMakesNoSeries924: celeris#924's family on a real server. One
// h2c connection sends n requests to one route, each with its own made-up
// :scheme. On epoll and io_uring the protocol layer accepts any :scheme value,
// and url.scheme (a default metric attribute) was that value: n series of
// http.server.request.duration, the same unbounded-series attack as the Host
// header. Every request must land in one series with url.scheme "http". std
// (x/net) resets such a stream, so it records nothing. The control sends
// :scheme http n times.
func TestH2SchemeMakesNoSeries924(t *testing.T) {
	const n = 50
	for _, tc := range []struct {
		name   string
		scheme func(i int) string
	}{
		{"made-up", func(i int) string { return fmt.Sprintf("x-made-up-%d", i) }},
		{"control-http", func(int) string { return "http" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, a := range keptArms(t) {
				if a.async {
					continue
				}
				t.Run(a.name, func(t *testing.T) {
					tp := sdktrace.NewTracerProvider()
					reader := sdkmetric.NewManualReader()
					mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
					defer func() {
						_ = tp.Shutdown(context.Background())
						_ = mp.Shutdown(context.Background())
					}()
					addr, stop := startKeptServer(t, func() *celeris.Server {
						srv := celeris.New(celeris.Config{Engine: a.engine, Protocol: celeris.Auto})
						srv.Use(celotel.New(celotel.Config{TracerProvider: tp, MeterProvider: mp}))
						srv.GET("/users", func(c *celeris.Context) error { return c.String(200, "ok") })
						return srv
					})
					defer stop()

					answered, reset := h2cRequests(t, addr, n, tc.scheme)

					var rm metricdata.ResourceMetrics
					if err := reader.Collect(context.Background(), &rm); err != nil {
						t.Fatal(err)
					}
					schemes := map[string]uint64{}
					points := 0
					for _, sm := range rm.ScopeMetrics {
						for _, m := range sm.Metrics {
							h, ok := m.Data.(metricdata.Histogram[float64])
							if m.Name != "http.server.request.duration" || !ok {
								continue
							}
							for _, dp := range h.DataPoints {
								points++
								v, _ := dp.Attributes.Value(attribute.Key("url.scheme"))
								schemes[v.AsString()] += dp.Count
							}
						}
					}
					t.Logf("%s: answered %d, reset %d of %d; duration series %d; url.scheme counts %v", a.name, answered, reset, n, points, schemes)
					if answered+reset != n {
						t.Fatalf("%d of %d streams ended", answered+reset, n)
					}
					if a.engine != celeris.Std || tc.name == "control-http" {
						// The native engines answer every stream, and so does
						// std for a valid :scheme.
						if answered != n {
							t.Errorf("%d of %d requests answered", answered, n)
						}
						if points != 1 || schemes["http"] != n {
							t.Errorf("%d series, url.scheme counts %v; want one series, \"http\" x %d", points, schemes, n)
						}
					} else if points > 1 {
						t.Errorf("std: %d series", points)
					}
				})
			}
		})
	}
}

// h2cRequests sends n GET /users requests on one prior-knowledge h2c
// connection, one at a time, with :scheme scheme(i), and counts the streams
// answered (END_STREAM from the server) and reset.
func h2cRequests(t *testing.T, addr string, n int, scheme func(i int) string) (answered, reset int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for i := range n {
		hb.Reset()
		for _, f := range []hpack.HeaderField{
			{Name: ":method", Value: "GET"},
			{Name: ":scheme", Value: scheme(i)},
			{Name: ":path", Value: "/users"},
			{Name: ":authority", Value: "api.example.com"},
		} {
			_ = enc.WriteField(f)
		}
		sid := uint32(2*i + 1)
		if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: sid, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
			t.Fatalf("stream %d: write headers: %v", sid, err)
		}
		for done := false; !done; {
			f, err := fr.ReadFrame()
			if err != nil {
				t.Fatalf("stream %d: read: %v (answered %d, reset %d)", sid, err, answered, reset)
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					_ = fr.WriteSettingsAck()
				}
			case *http2.GoAwayFrame:
				t.Fatalf("stream %d: GOAWAY %v (answered %d, reset %d)", sid, f.ErrCode, answered, reset)
			case *http2.RSTStreamFrame:
				if f.StreamID == sid {
					done, reset = true, reset+1
				}
			case *http2.HeadersFrame:
				if f.StreamID == sid && f.StreamEnded() {
					done, answered = true, answered+1
				}
			case *http2.DataFrame:
				if f.StreamID == sid && f.StreamEnded() {
					done, answered = true, answered+1
				}
			}
		}
	}
	return answered, reset
}
