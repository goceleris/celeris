//go:build linux

package epoll

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// largeBodyHandler returns a fixed N-byte body. Used to exercise the
// async write path with responses that accumulate cs.pendingBytes faster
// than any single flush can drain.
type largeBodyHandler struct{ body []byte }

func (h *largeBodyHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{
			{"content-type", "application/octet-stream"},
			{"content-length", fmt.Sprintf("%d", len(h.body))},
		},
		h.body)
}

// TestAsyncHandlerLargeBodyKeepAlive is a regression guard for the
// pendingBytes-leak bug in runAsyncHandler. Before the fix, every
// successful flush on the async path failed to reset cs.pendingBytes,
// so after cumulative response bytes crossed writeCap (4 MiB) every
// subsequent makeWriteFn call silently dropped the entire response and
// the client hung waiting for a reply that was never written.
//
// With a 64 KiB body and a single keep-alive conn, the bug manifested
// on request #65 (64 × 64 KiB = 4 MiB). This test pushes 200 requests
// on one conn — well past the trip point — and asserts every response
// comes back intact.
func TestAsyncHandlerLargeBodyKeepAlive(t *testing.T) {
	const bodySize = 64 * 1024
	body := bytes.Repeat([]byte{'a'}, bodySize)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:     addr,
		Protocol: engine.HTTP1,
		Resources: resource.Resources{
			Workers: 2,
		},
		AsyncHandlers: true,
	}
	e, err := New(cfg, &largeBodyHandler{body: body})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.Addr() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind in time")
	}
	target := e.Addr().String()

	conn, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(conn, 8192)

	// 200 serial requests — past the 64-request trip point of the bug.
	const N = 200
	for i := 0; i < N; i++ {
		if _, err := conn.Write([]byte("GET /large HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write req %d: %v", i, err)
		}
		if err := drainHTTPResponse(br, bodySize); err != nil {
			t.Fatalf("request %d: %v (trips around i=64 were the pendingBytes-leak fingerprint)", i, err)
		}
	}
}

// TestAsyncHandlerLargeBodyConcurrent stresses the async path with many
// concurrent conns doing large-body GETs. Matches the perfmatrix
// get-json-64k scenario that flagged the bug.
func TestAsyncHandlerLargeBodyConcurrent(t *testing.T) {
	const bodySize = 64 * 1024
	body := bytes.Repeat([]byte{'b'}, bodySize)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:     addr,
		Protocol: engine.HTTP1,
		Resources: resource.Resources{
			Workers: 4,
		},
		AsyncHandlers: true,
	}
	e, err := New(cfg, &largeBodyHandler{body: body})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	}()

	dl := time.Now().Add(3 * time.Second)
	for time.Now().Before(dl) && e.Addr() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind in time")
	}
	target := e.Addr().String()

	const conns = 8
	const perConn = 80 // each conn crosses the 64-request tripwire
	type res struct {
		id  int
		err error
		ok  int
	}
	ch := make(chan res, conns)
	for i := 0; i < conns; i++ {
		go func(id int) {
			tr := &http.Transport{
				MaxConnsPerHost:     1,
				MaxIdleConnsPerHost: 1,
				DisableCompression:  true,
				IdleConnTimeout:     10 * time.Second,
			}
			defer tr.CloseIdleConnections()
			cli := &http.Client{Transport: tr, Timeout: 3 * time.Second}
			url := "http://" + target + "/large"
			ok := 0
			for j := 0; j < perConn; j++ {
				resp, err := cli.Get(url)
				if err != nil {
					ch <- res{id, fmt.Errorf("conn %d req %d: %w", id, j, err), ok}
					return
				}
				n, err := io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					ch <- res{id, fmt.Errorf("conn %d req %d copy: %w", id, j, err), ok}
					return
				}
				if n != int64(bodySize) {
					ch <- res{id, fmt.Errorf("conn %d req %d: short body %d/%d", id, j, n, bodySize), ok}
					return
				}
				ok++
			}
			ch <- res{id, nil, ok}
		}(i)
	}

	for i := 0; i < conns; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("%v (ok=%d)", r.err, r.ok)
		}
		if r.ok != perConn {
			t.Fatalf("conn %d: only %d/%d succeeded", r.id, r.ok, perConn)
		}
	}
}

// drainHTTPResponse consumes one HTTP/1.1 response off br, validating
// it is a 200 with exactly expectBody body bytes.
func drainHTTPResponse(br *bufio.Reader, expectBody int) error {
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		return fmt.Errorf("unexpected status: %q", strings.TrimSpace(statusLine))
	}
	var cl int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			_, _ = fmt.Sscanf(line[len("content-length:"):], "%d", &cl)
		}
	}
	if cl != expectBody {
		return fmt.Errorf("content-length=%d want %d", cl, expectBody)
	}
	buf := make([]byte, cl)
	if _, err := io.ReadFull(br, buf); err != nil {
		return fmt.Errorf("read body (%d bytes): %w", cl, err)
	}
	return nil
}
