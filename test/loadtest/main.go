//go:build linux

// Package main runs load tests against all 36 celeris engine configurations.
// Must be run on Linux with io_uring support (kernel 5.10+).
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/adaptive"
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
	"github.com/goceleris/celeris/engine/std"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"

	"golang.org/x/net/http2"
)

// Test parameters
const (
	concurrency = 64
	duration    = 5 * time.Second
	startupWait = 1 * time.Second
)

var engines = []string{"iouring", "epoll", "adaptive", "std"}
var objectives = []resource.ObjectiveProfile{
	resource.LatencyOptimized,
	resource.ThroughputOptimized,
	resource.BalancedObjective,
}
var objectiveNames = []string{"latency", "throughput", "balanced"}
var protocols = []engine.Protocol{engine.HTTP1, engine.H2C, engine.Auto}
var protocolNames = []string{"h1", "h2", "hybrid"}

type testResult struct {
	name     string
	requests int64
	errors   int64
	duration time.Duration
	status   string // PASS, FAIL, SKIP
	detail   string
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	filter := os.Getenv("CELERIS_FILTER") // e.g., "iouring-latency-h1" or "epoll" or ""

	results := make([]testResult, 0, len(engines)*len(objectives)*len(protocols))

	for _, eng := range engines {
		for oi, obj := range objectives {
			for pi, proto := range protocols {
				name := fmt.Sprintf("celeris-%s-%s-%s", eng, objectiveNames[oi], protocolNames[pi])
				if filter != "" && !strings.Contains(name, filter) {
					continue
				}
				log.Printf("========== %s ==========", name)

				r := runTest(name, eng, obj, proto)
				results = append(results, r)

				status := r.status
				switch r.status {
				case "FAIL":
					status = "\033[31mFAIL\033[0m"
				case "PASS":
					status = "\033[32mPASS\033[0m"
				}
				log.Printf("[%s] %s: %d reqs, %d errs, %s — %s",
					status, r.name, r.requests, r.errors, r.duration.Round(time.Millisecond), r.detail)
			}
		}
	}

	// Summary
	fmt.Println("\n===== SUMMARY =====")
	var passed, failed, skipped int
	for _, r := range results {
		switch r.status {
		case "PASS":
			passed++
		case "FAIL":
			failed++
			fmt.Printf("  \033[31mFAIL\033[0m %s: %s\n", r.name, r.detail)
		case "SKIP":
			skipped++
		}
	}
	fmt.Printf("\n%d/%d passed, %d failed, %d skipped\n", passed, len(results), failed, skipped)

	if failed > 0 {
		os.Exit(1)
	}
}

type addrGetter interface {
	Addr() net.Addr
}

func runTest(name, engName string, obj resource.ObjectiveProfile, proto engine.Protocol) testResult {
	cfg := resource.Config{
		Addr:      ":0", // kernel-assigned port avoids conflicts between sequential tests
		Protocol:  proto,
		Objective: obj,
		Resources: resource.Resources{
			Preset: resource.Greedy,
		},
	}.WithDefaults()

	handler := newTestHandler()
	eng, err := createEngine(engName, cfg, handler)
	if err != nil {
		return testResult{name: name, status: "FAIL", detail: fmt.Sprintf("engine create: %v", err)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start engine in background
	listenErr := make(chan error, 1)
	go func() {
		listenErr <- eng.Listen(ctx)
	}()

	// Wait for engine to bind and report its address.
	ag, ok := eng.(addrGetter)
	if !ok {
		cancel()
		<-listenErr
		return testResult{name: name, status: "FAIL", detail: "engine does not implement Addr()"}
	}
	deadline := time.Now().Add(5 * time.Second)
	for ag.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-listenErr:
			cancel()
			if err != nil {
				return testResult{name: name, status: "FAIL", detail: fmt.Sprintf("engine Listen: %v", err)}
			}
			return testResult{name: name, status: "FAIL", detail: "engine exited without binding"}
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ag.Addr() == nil {
		cancel()
		<-listenErr
		return testResult{name: name, status: "FAIL", detail: "server failed to start within 5s"}
	}
	addr := ag.Addr().String()

	time.Sleep(100 * time.Millisecond) // small grace period

	// Determine which endpoints to test
	endpoints := []string{"/", "/json", "/users/42"}
	if proto == engine.HTTP1 || proto == engine.Auto {
		// H1 test
		reqs, errs, dur := loadTest(addr, endpoints, false)
		if errs > 0 || reqs == 0 {
			cancel()
			_ = eng.Shutdown(ctx)
			<-listenErr
			detail := fmt.Sprintf("H1 load: %d/%d errors", errs, reqs)
			if reqs == 0 {
				detail = "H1 load: 0 requests completed (server not responding)"
			}
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "FAIL", detail: detail}
		}
		if proto == engine.HTTP1 {
			cancel()
			_ = eng.Shutdown(ctx)
			<-listenErr
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "PASS",
				detail: fmt.Sprintf("H1: %d reqs, %.0f rps", reqs, float64(reqs)/dur.Seconds())}
		}
	}

	if proto == engine.H2C || proto == engine.Auto {
		// H2C test
		reqs, errs, dur := loadTest(addr, endpoints, true)
		cancel()
		_ = eng.Shutdown(ctx)
		<-listenErr
		if errs > 0 || reqs == 0 {
			detail := fmt.Sprintf("H2C load: %d/%d errors", errs, reqs)
			if reqs == 0 {
				detail = "H2C load: 0 requests completed (server not responding)"
			}
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "FAIL", detail: detail}
		}
		return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "PASS",
			detail: fmt.Sprintf("H2C: %d reqs, %.0f rps", reqs, float64(reqs)/dur.Seconds())}
	}

	cancel()
	_ = eng.Shutdown(ctx)
	<-listenErr
	return testResult{name: name, status: "PASS", detail: "completed"}
}

func createEngine(name string, cfg resource.Config, handler stream.Handler) (engine.Engine, error) {
	switch name {
	case "iouring":
		return iouring.New(cfg, handler)
	case "epoll":
		return epoll.New(cfg, handler)
	case "adaptive":
		return adaptive.New(cfg, handler)
	case "std":
		return std.New(cfg, handler)
	default:
		return nil, fmt.Errorf("unknown engine: %s", name)
	}
}



func loadTest(addr string, endpoints []string, h2c bool) (totalReqs, totalErrs int64, dur time.Duration) {
	var client *http.Client

	if h2c {
		// H2C client (unencrypted HTTP/2)
		client = &http.Client{
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				},
			},
			Timeout: 5 * time.Second,
		}
	} else {
		// HTTP/1.1 client with connection pooling
		client = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        concurrency * 2,
				MaxIdleConnsPerHost: concurrency * 2,
				MaxConnsPerHost:     concurrency * 2,
				IdleConnTimeout:     30 * time.Second,
			},
			Timeout: 5 * time.Second,
		}
	}
	defer client.CloseIdleConnections()

	var reqs, errs atomic.Int64
	var wg sync.WaitGroup

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	for i := range concurrency {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			ep := endpoints[workerID%len(endpoints)]
			scheme := "http"
			url := fmt.Sprintf("%s://%s%s", scheme, addr, ep)

			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() != nil {
						return // context cancelled, not a real error
					}
					errs.Add(1)
					reqs.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				if resp.StatusCode != 200 {
					errs.Add(1)
				}
				reqs.Add(1)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)
	return reqs.Load(), errs.Load(), elapsed
}

// Test handler - same endpoints as benchmark handler
func newTestHandler() stream.HandlerFunc {
	return func(_ context.Context, s *stream.Stream) error {
		defer s.Cancel()

		headers := s.GetHeaders()
		var method, path string
		for _, hdr := range headers {
			switch hdr[0] {
			case ":method":
				method = hdr[1]
			case ":path":
				path = hdr[1]
			}
		}

		switch {
		case method == "GET" && path == "/":
			return s.ResponseWriter.WriteResponse(s, 200,
				[][2]string{{"content-type", "text/plain"}},
				[]byte("Hello, World!"))
		case method == "GET" && path == "/json":
			return s.ResponseWriter.WriteResponse(s, 200,
				[][2]string{{"content-type", "application/json"}},
				[]byte(`{"message":"Hello, World!"}`))
		case method == "GET" && strings.HasPrefix(path, "/users/"):
			id := strings.TrimPrefix(path, "/users/")
			return s.ResponseWriter.WriteResponse(s, 200,
				[][2]string{{"content-type", "text/plain"}},
				[]byte("User ID: "+id))
		default:
			return s.ResponseWriter.WriteResponse(s, 404,
				[][2]string{{"content-type", "text/plain"}},
				[]byte("Not Found"))
		}
	}
}
