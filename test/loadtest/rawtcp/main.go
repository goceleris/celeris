//go:build linux

// Package main runs a raw TCP load test against celeris.Server to measure
// server-side performance without Go HTTP client overhead. Pre-formatted HTTP
// requests are sent over persistent TCP connections, and responses are consumed
// by scanning for Content-Length boundaries.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
)

const (
	concurrency = 64
	duration    = 5 * time.Second
	port        = "18090"
)

// Pre-formatted HTTP/1.1 requests (no allocation per iteration)
var (
	reqPlain = []byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
	reqJSON  = []byte("GET /json HTTP/1.1\r\nHost: localhost\r\n\r\n")
	reqParam = []byte("GET /users/42 HTTP/1.1\r\nHost: localhost\r\n\r\n")
)

type jsonMsg struct {
	Message string `json:"message"`
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	filter := os.Getenv("CELERIS_FILTER")

	engines := []celeris.EngineType{celeris.IOUring, celeris.Epoll}
	engineNames := []string{"iouring", "epoll"}
	objectives := []celeris.Objective{celeris.Latency, celeris.Throughput, celeris.Balanced}
	objectiveNames := []string{"latency", "throughput", "balanced"}

	type result struct {
		name string
		rps  float64
		errs int64
	}
	var results []result

	for ei, eng := range engines {
		for oi, obj := range objectives {
			name := fmt.Sprintf("%s-%s", engineNames[ei], objectiveNames[oi])
			if filter != "" && !strings.Contains(name, filter) {
				continue
			}
			rps, errs := runTest(name, eng, obj)
			results = append(results, result{name, rps, errs})
			status := "PASS"
			if errs > 0 {
				status = "FAIL"
			}
			log.Printf("[%s] %s: %.0f rps, %d errors", status, name, rps, errs)
		}
	}

	fmt.Println("\n===== SUMMARY =====")
	for _, r := range results {
		fmt.Printf("  %-30s %10.0f rps  %d errors\n", r.name, r.rps, r.errs)
	}
}

func runTest(name string, eng celeris.EngineType, obj celeris.Objective) (float64, int64) {
	s := celeris.New(celeris.Config{
		Addr:           ":" + port,
		Protocol:       celeris.HTTP1,
		Engine:         eng,
		Objective:      obj,
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

	addr := "127.0.0.1:" + port
	if !waitForReady(addr, 5*time.Second) {
		cancel()
		<-listenErr
		log.Printf("[FAIL] %s: server failed to start", name)
		return 0, 1
	}
	time.Sleep(100 * time.Millisecond)

	requests := [][]byte{reqPlain, reqJSON, reqParam}
	var totalReqs, totalErrs atomic.Int64
	var wg sync.WaitGroup

	deadline := time.Now().Add(duration)

	for i := range concurrency {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			req := requests[workerID%len(requests)]

			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				totalErrs.Add(1)
				return
			}
			defer func() { _ = conn.Close() }()

			reader := bufio.NewReaderSize(conn, 4096)
			buf := make([]byte, 4096)

			for time.Now().Before(deadline) {
				// Send request
				if _, err := conn.Write(req); err != nil {
					totalErrs.Add(1)
					return
				}

				// Read response: scan for \r\n\r\n then read body
				if err := consumeResponse(reader, buf); err != nil {
					totalErrs.Add(1)
					return
				}
				totalReqs.Add(1)
			}
		}(i)
	}

	wg.Wait()
	elapsed := duration
	reqs := totalReqs.Load()
	errs := totalErrs.Load()
	rps := float64(reqs) / elapsed.Seconds()

	cancel()
	<-listenErr
	return rps, errs
}

// consumeResponse reads a full HTTP/1.1 response from the reader.
// It scans for the header/body boundary and Content-Length to know how much body to read.
func consumeResponse(r *bufio.Reader, scratch []byte) error {
	contentLength := 0
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return err
		}
		// End of headers
		if len(line) <= 2 {
			break
		}
		// Parse content-length
		if len(line) > 16 && (line[0] == 'c' || line[0] == 'C') {
			lower := strings.ToLower(string(line[:15]))
			if lower == "content-length:" {
				n := 0
				for i := 15; i < len(line); i++ {
					b := line[i]
					if b >= '0' && b <= '9' {
						n = n*10 + int(b-'0')
					}
				}
				contentLength = n
			}
		}
	}
	// Read body
	if contentLength > 0 {
		if contentLength <= len(scratch) {
			_, err := io.ReadFull(r, scratch[:contentLength])
			return err
		}
		_, err := io.CopyN(io.Discard, r, int64(contentLength))
		return err
	}
	return nil
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
