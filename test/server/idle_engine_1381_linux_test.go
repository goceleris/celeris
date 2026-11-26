//go:build linux

package celeris_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#1381. A native-engine server with no connection waits longer between
// wake-ups than it did (100 ms where an epoll loop waited 4 ms). What it must
// not cost, through the public API: the first request after an idle, and the
// time an idle server takes to shut down. Both are medians over several cycles
// with a generous bound (the engines' own tests hold the tight ones): they
// would be many times over it only if an idle loop were not woken by what
// reaches it. The values are logged for the before/after comparison.

const (
	idleCycles1381     = 3
	idleGap1381        = 700 * time.Millisecond
	idleFirstBound1381 = 250 * time.Millisecond
	idleStopBound1381  = 500 * time.Millisecond
)

func TestIdleServerFirstRequestAndShutdown1381(t *testing.T) {
	for _, tc := range []struct {
		name string
		eng  celeris.EngineType
	}{
		{"epoll", celeris.Epoll},
		{"iouring", celeris.IOUring},
		{"adaptive", celeris.Adaptive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var firsts, stops []time.Duration
			for range idleCycles1381 {
				first, stop := idleCycle1381(t, tc.eng)
				firsts = append(firsts, first)
				stops = append(stops, stop)
			}
			mf, ms := median1381(firsts), median1381(stops)
			t.Logf("celeris1381 %s: first request after %v idle %v (median %v, bound %v); idle Shutdown to Start returning %v (median %v, bound %v)",
				tc.name, idleGap1381, firsts, mf, idleFirstBound1381, stops, ms, idleStopBound1381)
			if mf > idleFirstBound1381 {
				t.Errorf("%s: the first request after an idle took a median of %v, want at most %v", tc.name, mf, idleFirstBound1381)
			}
			if ms > idleStopBound1381 {
				t.Errorf("%s: an idle server took a median of %v to shut down, want at most %v", tc.name, ms, idleStopBound1381)
			}
		})
	}
}

func median1381(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)/2]
}

// idleCycle1381 starts a server, leaves it idle for idleGap1381, times a request
// on a new connection, leaves it idle again, and times its Shutdown.
func idleCycle1381(t *testing.T, eng celeris.EngineType) (first, stop time.Duration) {
	t.Helper()
	var (
		s         *celeris.Server
		addr      string
		startDone chan error
	)
	startRetrying1104(t, "server start", func() error {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr = ln.Addr().String()
		if cerr := ln.Close(); cerr != nil {
			t.Fatalf("close probe listener: %v", cerr)
		}
		s = celeris.New(celeris.Config{Engine: eng, Addr: addr})
		token := fmt.Sprintf("ok-%d", time.Now().UnixNano())
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "%s", token) })
		startDone = make(chan error, 1)
		go func(s *celeris.Server, done chan error) { done <- s.Start() }(s, startDone)
		ok, werr := waitReadyErr(&http.Client{Timeout: 300 * time.Millisecond}, "http://"+addr+"/ping", token, startDone)
		if werr != nil {
			return fmt.Errorf("Start returned before the server was ready: %w", werr)
		}
		if !ok {
			t.Fatalf("%s: server never became ready on %s", eng, addr)
		}
		return nil
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	// A client of its own, with no connection kept: every request is a new one.
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	time.Sleep(idleGap1381) // idle: the loops reach their longest wait
	t0 := time.Now()
	resp, err := client.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("%s: GET /ping: %v", eng, err)
	}
	_ = resp.Body.Close()
	first = time.Since(t0)

	time.Sleep(idleGap1381)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t0 = time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("%s: Shutdown: %v", eng, err)
	}
	select {
	case err := <-startDone:
		stop = time.Since(t0)
		if err != nil {
			t.Errorf("%s: Start returned %v", eng, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: Start did not return 15 s after Shutdown", eng)
	}
	return first, stop
}
