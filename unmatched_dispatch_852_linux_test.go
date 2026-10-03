//go:build linux

package celeris_test

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// pingers852 is how many fresh connections ask GET /ping while an unmatched
// request's global middleware blocks. With two workers, the chance that none
// of them lands on the blocked one is 2^-16.
const pingers852 = 16

// TestUnmatchedBlockingChainLeavesWorkerFree852 pins the dispatch half of
// celeris#852 on the native engines, over HTTP/1.1 and h2c. Under
// AsyncHandlers, a global middleware that blocks on an unmatched request (a
// remote session store, an auth upstream) must not hold the engine worker,
// and every connection on it, as it would not on a route inheriting that
// default. One unmatched request whose middleware sleeps 10 ms promotes the
// unmatched chain; then an unmatched request's middleware waits, for up to
// 3 s, until pingers852 fresh connections have each been answered GET /ping.
// Run inline on a worker, it holds every connection that worker owns, so it
// waits the full 3 s.
func TestUnmatchedBlockingChainLeavesWorkerFree852(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		for _, proto := range []string{"h1", "h2c"} {
			t.Run(e.name+"/"+proto, func(t *testing.T) { checkUnmatchedOffWorker852(t, e.eng, proto) })
		}
	}
}

func checkUnmatchedOffWorker852(t *testing.T, eng celeris.EngineType, proto string) {
	var pinged atomic.Int32
	var once sync.Once
	waiting := make(chan struct{})
	waited := make(chan int32, 1) // pings answered when the wait ended
	addr := startServerConfig852(t, celeris.Config{Engine: eng, AsyncHandlers: true, Workers: 2}, func(s *celeris.Server) {
		s.Use(func(c *celeris.Context) error {
			switch c.Path() {
			case "/warm-852":
				time.Sleep(10 * time.Millisecond)
			case "/wait-852":
				once.Do(func() { close(waiting) })
				deadline := time.Now().Add(3 * time.Second)
				for pinged.Load() < pingers852 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				select {
				case waited <- pinged.Load():
				default:
				}
			}
			return c.Next()
		})
		s.GET("/ping", func(c *celeris.Context) error {
			pinged.Add(1)
			return c.String(200, "pong")
		})
	})

	unmatched := func(path string) error {
		if proto == "h2c" {
			ended, _, err := h2StreamEnds837(addr, path, 10*time.Second)
			if err == nil && !ended {
				err = fmt.Errorf("stream did not end")
			}
			return err
		}
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			return err
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 404 {
			return fmt.Errorf("status %d, want 404", resp.StatusCode)
		}
		return nil
	}

	if err := unmatched("/warm-852"); err != nil {
		t.Fatalf("GET /warm-852: %v", err)
	}
	aDone := make(chan error, 1)
	go func() { aDone <- unmatched("/wait-852") }()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the middleware never saw GET /wait-852")
	}

	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var wg sync.WaitGroup
	lat := make([]time.Duration, pingers852)
	errs := make([]error, pingers852)
	for i := range pingers852 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			resp, err := cl.Get("http://" + addr + "/ping")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode != 200 {
					err = fmt.Errorf("status %d", resp.StatusCode)
				}
			}
			lat[i], errs[i] = time.Since(start), err
		}()
	}
	wg.Wait()

	var answered int32
	select {
	case answered = <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting middleware never finished")
	}
	if err := <-aDone; err != nil {
		t.Errorf("GET /wait-852: %v", err)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("ping %d: %v", i, err)
		}
	}
	var worst time.Duration
	for _, d := range lat {
		worst = max(worst, d)
	}
	if answered != pingers852 {
		t.Errorf("the unmatched request's blocking middleware held an engine worker: %d of %d pings answered while it waited 3 s (slowest ping %v)",
			answered, pingers852, worst)
	}
	t.Logf("pings answered while the unmatched request waited: %d of %d; slowest ping %v", answered, pingers852, worst)
}
