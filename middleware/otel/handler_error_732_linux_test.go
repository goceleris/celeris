//go:build linux

package otel_test

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/goceleris/celeris"
	celotel "github.com/goceleris/celeris/middleware/otel"
)

// TestHandlerErrorSurvivesNextRequest pins the error site of the otel
// middleware for celeris#732.
//
// When the handler returns an error, the span records it twice: an
// exception event whose exception.message is the error's message, and the
// error status, whose description is the message too. The span processor
// keeps both until it exports the span. A message can be a request string,
// errors.New(c.Param("id")) or an HTTPError whose Message is a header, and
// on epoll and io_uring (and Adaptive, which runs them) that is a view of
// the connection's receive buffer. Three requests per route with the same
// layout and different values go over one keep-alive connection; every
// span must keep its own request's message. The middleware records the
// error at two sites, with metrics and without, so both are run.
func TestHandlerErrorSurvivesNextRequest(t *testing.T) {
	for _, metrics := range []bool{true, false} {
		mode := "metrics"
		if !metrics {
			mode = "no-metrics"
		}
		for _, a := range mwArms(t) {
			t.Run(mode+"/"+a.name, func(t *testing.T) {
				exp := tracetest.NewInMemoryExporter()
				cfg := celotel.Config{TracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))}
				if metrics {
					cfg.MeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
				} else {
					cfg.DisableMetrics = true
				}
				addr, stop := startKeptServer(t, func() *celeris.Server {
					srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
					// Outside otel: answers 200, so the connection carries
					// on, once otel has recorded the error.
					srv.Use(func(c *celeris.Context) error {
						if err := c.Next(); err != nil && !c.IsWritten() {
							return c.String(200, "ok")
						}
						return nil
					})
					srv.Use(celotel.New(cfg))
					srv.GET("/e/:id", func(c *celeris.Context) error { return errors.New(c.Param("id")) })
					srv.GET("/h/:id", func(c *celeris.Context) error { return celeris.NewHTTPError(400, c.Header("x-err")) })
					return srv
				})
				defer stop()

				// want maps a request's path, which the span keeps as a copy
				// (url.path), to the exception type and message its span must
				// keep.
				want := map[string][2]string{}
				for _, route := range []string{"e", "h"} {
					conn, br := keptDial(t, addr)
					for i := range 3 {
						v := strings.Repeat(string(rune('a'+i)), 4)
						path := "/" + route + "/id-" + v
						keptRoundTrip(t, conn, br, "GET "+path+" HTTP/1.1\r\nHost: h\r\nX-Err: err-"+v+"\r\n\r\n")
						if route == "e" {
							want[path] = [2]string{"*errors.errorString", "id-" + v}
						} else {
							want[path] = [2]string{"*celeris.HTTPError", "code=400, message=err-" + v}
						}
					}
					_ = conn.Close()
				}

				// The middleware ends a span after the response reaches the client.
				spans := exp.GetSpans()
				for i := 0; len(spans) < 6 && i < 200; i++ {
					time.Sleep(10 * time.Millisecond)
					spans = exp.GetSpans()
				}
				if len(spans) != 6 {
					t.Fatalf("got %d spans, want 6", len(spans))
				}
				sort.Slice(spans, func(i, j int) bool { return spans[i].StartTime.Before(spans[j].StartTime) })
				var wrong []string
				for i, s := range spans {
					path := spanAttr(s.Attributes, "url.path")
					w, ok := want[path]
					if !ok {
						t.Fatalf("span %d has url.path %q, not one of the requests", i+1, path)
					}
					var events []string
					for _, ev := range s.Events {
						if ev.Name == "exception" {
							events = append(events, spanAttr(ev.Attributes, "exception.type")+" "+spanAttr(ev.Attributes, "exception.message"))
						}
					}
					if got, wantEv := strings.Join(events, ","), w[0]+" "+w[1]; got != wantEv {
						wrong = append(wrong, fmt.Sprintf("span %d (%s) exception event %q (want %q)", i+1, path, got, wantEv))
					}
					if s.Status.Description != w[1] {
						wrong = append(wrong, fmt.Sprintf("span %d (%s) status description %q (want %q)", i+1, path, s.Status.Description, w[1]))
					}
				}
				t.Logf("MW732OTELERR mode=%s arm=%s spans=%d wrong=%d", mode, a.name, len(spans), len(wrong))
				if len(wrong) > 0 {
					t.Errorf("a span keeps the handler's error message as a view, which reads other bytes after the connection's later requests:\n  %s", strings.Join(wrong, "\n  "))
				}
			})
		}
	}
}

func spanAttr(kvs []attribute.KeyValue, key attribute.Key) string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value.AsString()
		}
	}
	return ""
}
