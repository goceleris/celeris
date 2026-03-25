//go:build linux

// Package main runs full-stack load tests through the celeris.Server application
// layer (router, Context, JSON encoding, response writing) across all engine configs.
//
// H1 uses raw TCP pipelining (16 requests in flight per connection) to saturate
// the server. H2 uses concurrent streams per connection via goroutine fan-out.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"

	"golang.org/x/net/http2"
)

const (
	h1Connections    = 128 // number of TCP connections for H1
	h1PipelineDepth  = 16  // requests sent before reading responses
	h2Connections    = 64  // number of TCP connections for H2
	h2StreamsPerConn = 100 // concurrent streams per H2 connection
)

var duration = func() time.Duration {
	if d := os.Getenv("DURATION"); d != "" {
		if dur, err := time.ParseDuration(d); err == nil {
			return dur
		}
	}
	return 5 * time.Second
}()

var engines = []celeris.EngineType{celeris.IOUring, celeris.Epoll, celeris.Std}
var engineNames = []string{"iouring", "epoll", "std"}
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

	results := make([]testResult, 0, len(engines)*len(protocols))

	for ei, eng := range engines {
		for pi, proto := range protocols {
			name := fmt.Sprintf("celeris-%s-%s", engineNames[ei], protocolNames[pi])
			if filter != "" && !strings.Contains(name, filter) {
				continue
			}
			log.Printf("========== %s ==========", name)
			r := runTest(name, eng, proto)
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

func runTest(name string, eng celeris.EngineType, proto celeris.Protocol) testResult {
	s := celeris.New(celeris.Config{
		Addr:           ":0",
		Protocol:       proto,
		Engine:         eng,
		DisableMetrics: true,
	})

	s.GET("/", func(c *celeris.Context) error {
		return c.String(200, "Hello, World!")
	})
	s.GET("/json", func(c *celeris.Context) error {
		return c.JSON(200, jsonMsg{Message: "Hello, World!"})
	})
	s.GET("/users/:id", func(c *celeris.Context) error {
		return c.String(200, "%s", "User ID: "+c.Param("id"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenErr := make(chan error, 1)
	go func() {
		listenErr <- s.StartWithContext(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for s.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-listenErr:
			cancel()
			if err != nil {
				return testResult{name: name, status: "FAIL", detail: fmt.Sprintf("server start: %v", err)}
			}
			return testResult{name: name, status: "FAIL", detail: "server exited without binding"}
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.Addr() == nil {
		cancel()
		<-listenErr
		return testResult{name: name, status: "FAIL", detail: "server failed to start within 5s"}
	}
	addr := s.Addr().String()
	time.Sleep(100 * time.Millisecond)

	endpoints := []string{"/", "/json", "/users/42"}
	if proto == celeris.HTTP1 || proto == celeris.Auto {
		reqs, errs, dur := loadTestH1Pipelined(addr, endpoints)
		if errs > reqs/100 || reqs == 0 {
			cancel()
			<-listenErr
			detail := fmt.Sprintf("H1 load: %d/%d errors", errs, reqs)
			if reqs == 0 {
				detail = "H1 load: 0 requests completed"
			}
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "FAIL", detail: detail}
		}
		if proto == celeris.HTTP1 {
			cancel()
			<-listenErr
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "PASS",
				detail: fmt.Sprintf("H1: %d reqs, %.0f rps", reqs, float64(reqs)/dur.Seconds())}
		}
	}

	if proto == celeris.H2C || proto == celeris.Auto {
		reqs, errs, dur := loadTestH2Concurrent(addr, endpoints)
		cancel()
		<-listenErr
		if errs > reqs/100 || reqs == 0 {
			detail := fmt.Sprintf("H2C load: %d/%d errors", errs, reqs)
			if reqs == 0 {
				detail = "H2C load: 0 requests completed"
			}
			return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "FAIL", detail: detail}
		}
		return testResult{name: name, requests: reqs, errors: errs, duration: dur, status: "PASS",
			detail: fmt.Sprintf("H2C: %d reqs, %.0f rps", reqs, float64(reqs)/dur.Seconds())}
	}

	cancel()
	<-listenErr
	return testResult{name: name, status: "PASS", detail: "completed"}
}

// loadTestH1Pipelined uses raw TCP connections with HTTP/1.1 pipelining.
// Each connection sends h1PipelineDepth requests before reading responses,
// achieving ~16x higher throughput than serial request/response.
func loadTestH1Pipelined(addr string, endpoints []string) (totalReqs, totalErrs int64, dur time.Duration) {
	var reqs, errs atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	for i := range h1Connections {
		wg.Add(1)
		go func(connID int) {
			defer wg.Done()
			ep := endpoints[connID%len(endpoints)]
			h1PipelineWorker(ctx, addr, ep, &reqs, &errs)
		}(i)
	}

	wg.Wait()
	return reqs.Load(), errs.Load(), time.Since(start)
}

// h1PipelineWorker runs pipelined HTTP/1.1 on a single TCP connection.
func h1PipelineWorker(ctx context.Context, addr, path string, reqs, errs *atomic.Int64) {
	reqLine := "GET " + path + " HTTP/1.1\r\nHost: localhost\r\n\r\n"
	reqBytes := []byte(reqLine)

	for ctx.Err() == nil {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			errs.Add(1)
			reqs.Add(1)
			time.Sleep(10 * time.Millisecond)
			continue
		}

		reader := bufio.NewReaderSize(conn, 65536)
		alive := true
		for alive && ctx.Err() == nil {
			// Send pipeline batch.
			for range h1PipelineDepth {
				if _, err := conn.Write(reqBytes); err != nil {
					errs.Add(int64(h1PipelineDepth))
					reqs.Add(int64(h1PipelineDepth))
					alive = false
					break
				}
			}
			if !alive {
				break
			}

			// Read pipeline batch responses.
			for range h1PipelineDepth {
				if !readH1Response(reader) {
					errs.Add(1)
					alive = false
					break
				}
				reqs.Add(1)
			}
		}
		_ = conn.Close()
	}
}

// readH1Response reads a single HTTP/1.1 response from a buffered reader.
// Returns true on success, false on error.
func readH1Response(r *bufio.Reader) bool {
	// Read status line.
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	if len(line) < 12 {
		return false
	}

	// Read headers until blank line.
	contentLength := -1
	for {
		line, err = r.ReadString('\n')
		if err != nil {
			return false
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		// Parse content-length.
		if len(line) > 16 && (line[0] == 'c' || line[0] == 'C') {
			lower := strings.ToLower(line)
			if strings.HasPrefix(lower, "content-length:") {
				val := strings.TrimSpace(line[15:])
				if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
					contentLength = n
				}
			}
		}
	}

	// Read body.
	if contentLength > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(contentLength)); err != nil {
			return false
		}
	}
	return true
}

// loadTestH2Concurrent uses the Go H2 transport with high concurrency.
// Each connection runs h2StreamsPerConn goroutines sending concurrent requests,
// exercising H2 stream multiplexing.
func loadTestH2Concurrent(addr string, endpoints []string) (totalReqs, totalErrs int64, dur time.Duration) {
	var reqs, errs atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	// Create multiple independent H2 transports (each maintains one TCP connection).
	for c := range h2Connections {
		transport := &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
		client := &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		}

		// Fan out streams per connection.
		for s := range h2StreamsPerConn {
			wg.Add(1)
			go func(connID, streamID int) {
				defer wg.Done()
				ep := endpoints[(connID*h2StreamsPerConn+streamID)%len(endpoints)]
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
			}(c, s)
		}
	}

	wg.Wait()
	return reqs.Load(), errs.Load(), time.Since(start)
}
