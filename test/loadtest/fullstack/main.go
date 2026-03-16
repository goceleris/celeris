//go:build linux

// Package main runs full-stack load tests through the celeris.Server application
// layer (router, Context, JSON encoding, response writing) across all engine configs.
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

	"github.com/goceleris/celeris"

	"golang.org/x/net/http2"
)

const (
	concurrency = 64
	duration    = 5 * time.Second
	port        = "18080"
)

var engines = []celeris.EngineType{celeris.IOUring, celeris.Epoll, celeris.Std}
var engineNames = []string{"iouring", "epoll", "std"}
var objectives = []celeris.Objective{celeris.Latency, celeris.Throughput, celeris.Balanced}
var objectiveNames = []string{"latency", "throughput", "balanced"}
var protocols = []celeris.Protocol{celeris.HTTP1, celeris.H2C, celeris.Auto}
var protocolNames = []string{"h1", "h2", "hybrid"}

type jsonMsg struct {
	Message string `json:"message"`
}

type testResult struct {
	name     string
	requests int64
	errors   int64
	duration time.Duration
	status   string
	detail   string
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	filter := os.Getenv("CELERIS_FILTER")

	results := make([]testResult, 0, len(engines)*len(objectives)*len(protocols))

	for ei, eng := range engines {
		for oi, obj := range objectives {
			for pi, proto := range protocols {
				name := fmt.Sprintf("celeris-%s-%s-%s", engineNames[ei], objectiveNames[oi], protocolNames[pi])
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

	fmt.Println("\n===== SUMMARY =====")
	var passed, failed int
	for _, r := range results {
		switch r.status {
		case "PASS":
			passed++
		case "FAIL":
			failed++
			fmt.Printf("  \033[31mFAIL\033[0m %s: %s\n", r.name, r.detail)
		}
	}
	fmt.Printf("\n%d/%d passed, %d failed\n", passed, len(results), failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func runTest(name string, eng celeris.EngineType, obj celeris.Objective, proto celeris.Protocol) testResult {
	s := celeris.New(celeris.Config{
		Addr:           ":" + port,
		Protocol:       proto,
		Engine:         eng,
		Objective:      obj,
		DisableMetrics: true,
	})

	// Static route — exercises static fast-path lookup
	s.GET("/", func(c *celeris.Context) error {
		return c.String(200, "Hello, World!")
	})

	// JSON route — exercises JSON encoder pool + itoa + response headers
	s.GET("/json", func(c *celeris.Context) error {
		return c.JSON(200, jsonMsg{Message: "Hello, World!"})
	})

	// Parameterized route — exercises radix trie lookup
	s.GET("/users/:id", func(c *celeris.Context) error {
		return c.String(200, "%s", "User ID: "+c.Param("id"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenErr := make(chan error, 1)
	go func() {
		listenErr <- s.StartWithContext(ctx)
	}()

	addr := "127.0.0.1:" + port
	if !waitForReady(addr, 5*time.Second) {
		cancel()
		<-listenErr
		return testResult{name: name, status: "FAIL", detail: "server failed to start within 5s"}
	}
	time.Sleep(100 * time.Millisecond)

	endpoints := []string{"/", "/json", "/users/42"}
	if proto == celeris.HTTP1 || proto == celeris.Auto {
		reqs, errs, dur := loadTest(addr, endpoints, false)
		if errs > 0 {
			cancel()
			<-listenErr
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "FAIL",
				detail: fmt.Sprintf("H1 load: %d/%d errors", errs, reqs)}
		}
		if proto == celeris.HTTP1 {
			cancel()
			<-listenErr
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "PASS",
				detail: fmt.Sprintf("H1: %d reqs, %.0f rps", reqs, float64(reqs)/dur.Seconds())}
		}
	}

	if proto == celeris.H2C || proto == celeris.Auto {
		reqs, errs, dur := loadTest(addr, endpoints, true)
		cancel()
		<-listenErr
		if errs > 0 {
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "FAIL",
				detail: fmt.Sprintf("H2C load: %d/%d errors", errs, reqs)}
		}
		return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "PASS",
			detail: fmt.Sprintf("H2C: %d reqs, %.0f rps", reqs, float64(reqs)/dur.Seconds())}
	}

	cancel()
	<-listenErr
	return testResult{name: name, status: "PASS", detail: "completed"}
}

func waitForReady(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func loadTest(addr string, endpoints []string, h2c bool) (totalReqs, totalErrs int64, dur time.Duration) {
	var client *http.Client
	if h2c {
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
			url := fmt.Sprintf("http://%s%s", addr, ep)
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
						return
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
