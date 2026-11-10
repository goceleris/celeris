//go:build linux

package sse_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// An SSE stream over h2c on an async route (celeris#889 item 34). Probe on main
// (0897add): the headers and the first event reach the client within a few
// milliseconds on std, epoll and Adaptive when the route is .Async(); on a route
// that is not (the default, also with Config.AsyncHandlers) they reach it when
// the handler returns, about 3 s into a 3 s stream, because an inline HTTP/2
// handler runs on the event loop and the engine sends the response when it
// returns. Flush cannot change that, so nothing is flushed for a detached
// stream: the route has to be .Async(). These cells pin the async route.

const h2cTick889 = 150 * time.Millisecond

func h2cClient889(addr string) *http.Transport {
	h2only := new(http.Protocols)
	h2only.SetUnencryptedHTTP2(true)
	return &http.Transport{Protocols: h2only, DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, n, addr)
	}}
}

type h2cEngine889 struct {
	name string
	kind celeris.EngineType
}

func h2cEngines889(t *testing.T) []h2cEngine889 {
	t.Helper()
	engines := []h2cEngine889{{"std", celeris.Std}}
	for _, k := range sseNativeEngineKinds(t) {
		engines = append(engines, h2cEngine889{k.String(), k})
	}
	return append(engines, h2cEngine889{"adaptive", celeris.Adaptive})
}

// tickHandler889 sends a tick every h2cTick889 for ten ticks and records when
// it returned.
func tickHandler889(returned *atomic.Bool) celeris.HandlerFunc {
	return sse.New(sse.Config{
		HeartbeatInterval: -1,
		Handler: func(c *sse.Client) {
			defer returned.Store(true)
			for range 10 {
				if c.Send(sse.Event{Data: "tick"}) != nil {
					return
				}
				select {
				case <-c.Context().Done():
					return
				case <-time.After(h2cTick889):
				}
			}
		},
	})
}

// TestSSEOverH2CStreamsOnAnAsyncRoute889: the response headers, and then each
// event, reach an h2c client while the handler is still running.
func TestSSEOverH2CStreamsOnAnAsyncRoute889(t *testing.T) {
	for _, e := range h2cEngines889(t) {
		t.Run(e.name, func(t *testing.T) {
			var returned atomic.Bool
			addr, shutdown := startSSEServer(t, e.kind, false, true, tickHandler889(&returned))
			defer shutdown()
			tr := h2cClient889(addr)
			defer tr.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/events", nil)
			req.Header.Set("Accept", "text/event-stream")
			began := time.Now()
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("no response headers: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			head := time.Since(began)
			if resp.ProtoMajor != 2 || resp.StatusCode != 200 {
				t.Fatalf("proto %s status %d, want HTTP/2 200", resp.Proto, resp.StatusCode)
			}
			br := bufio.NewReader(resp.Body)
			var stamps []time.Duration
			for len(stamps) < 3 {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("after %d events: %v", len(stamps), err)
				}
				if strings.HasPrefix(line, "data:") {
					stamps = append(stamps, time.Since(began))
				}
			}
			t.Logf("headers after %v; events after %v", head.Round(time.Millisecond), stamps)
			// A response that goes out when the handler returns (what an inline
			// route does) shows the headers after the ten ticks, 1.5 s.
			if head > 700*time.Millisecond {
				t.Errorf("response headers after %v, want them while the handler runs (an event every %v)", head, h2cTick889)
			}
			if stamps[0] > 700*time.Millisecond || stamps[2] < stamps[0]+h2cTick889 {
				t.Errorf("events at %v: want the first early and the third at least one tick after it", stamps)
			}
			if returned.Load() {
				t.Error("the handler had returned when the third event arrived")
			}
		})
	}
}

// TestSSEOverH2CAsyncRouteDoesNotHoldTheLoop889: while an h2c stream on an async
// route is open, other connections are answered at once (HEAD /events is the
// bystander: the middleware answers it with the headers). With Workers 2, 12
// fresh connections reach both loops. An inline route holds its loop for the
// stream's life (probe: 4 of 12 such requests took 0.5 to 2.7 s on epoll).
func TestSSEOverH2CAsyncRouteDoesNotHoldTheLoop889(t *testing.T) {
	for _, e := range h2cEngines889(t) {
		t.Run(e.name, func(t *testing.T) {
			var returned atomic.Bool
			addr, shutdown := startSSEServerCfg(t, celeris.Config{Engine: e.kind, Workers: 2, ShutdownTimeout: 2 * time.Second}, true, tickHandler889(&returned))
			defer shutdown()
			tr := h2cClient889(addr)
			defer tr.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/events", nil)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("no response headers: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
			var worst time.Duration
			for range 12 {
				st := time.Now()
				r, err := cl.Head("http://" + addr + "/events")
				if err != nil {
					t.Fatalf("bystander HEAD: %v", err)
				}
				_ = r.Body.Close()
				worst = max(worst, time.Since(st))
			}
			t.Logf("12 bystander HEADs while the h2c stream is open: worst %v", worst.Round(time.Millisecond))
			if returned.Load() {
				t.Fatal("the stream ended before the bystanders ran: the measurement says nothing") // it lasts 1.5 s
			}
			if worst > 500*time.Millisecond {
				t.Errorf("a bystander waited %v while an async-route h2c stream was open", worst)
			}
		})
	}
}
