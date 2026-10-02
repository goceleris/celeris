//go:build linux

package epoll

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// asyncDispatchBenchHandler answers every request on the dispatch goroutine.
type asyncDispatchBenchHandler struct{}

func (asyncDispatchBenchHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (asyncDispatchBenchHandler) RouteAsync(_, _ string) bool { return true }
func (asyncDispatchBenchHandler) HasAsyncRoutes() bool        { return true }

// BenchmarkAsyncDispatchKeepAlive is one keep-alive client against an async
// route, a request at a time: every iteration is one pass of the dispatch
// goroutine's loop (serveAsync: the park and wake, detachMu around ProcessH1,
// the direct write). It is the per-request path celeris#791's lock-ownership
// flag sits on, and it carries the loopback round trip, so it says whether
// that path got slower by anything a request can see.
func BenchmarkAsyncDispatchKeepAlive(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		AsyncHandlers: true,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, asyncDispatchBenchHandler{})
	if err != nil {
		b.Fatalf("epoll engine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			b.Error("engine did not stop within 5s")
		}
	}()
	var c net.Conn
	for deadline := time.Now().Add(8 * time.Second); ; {
		if c, err = net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil && e.NumWorkers() > 0 {
			break
		}
		if c != nil {
			_ = c.Close()
		}
		if time.Now().After(deadline) {
			b.Fatal("engine did not start listening within 8s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() { _ = c.Close() }()
	req := []byte("GET /a HTTP/1.1\r\nHost: x\r\n\r\n")
	// The first response fixes the length every later one has (the Date
	// header is fixed-width), so the loop reads exactly that many bytes.
	br := bufio.NewReader(c)
	if _, err := c.Write(req); err != nil {
		b.Fatal(err)
	}
	n, err := rawResponseLen(br)
	if err != nil {
		b.Fatalf("first response: %v", err)
	}
	buf := make([]byte, n)
	for range 200 { // warm the conn's dispatch goroutine and buffers
		if _, err := c.Write(req); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(br, buf); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := c.Write(req); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(br, buf); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if string(buf[:15]) != "HTTP/1.1 200 OK" {
		b.Fatalf("last response %q", buf[:min(n, 40)])
	}
}

// rawResponseLen consumes one HTTP/1.1 response from br and returns its length
// on the wire: the header block through its blank line, then content-length
// bytes of body.
func rawResponseLen(br *bufio.Reader) (int, error) {
	n, cl := 0, -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		n += len(line)
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "content-length") {
			if cl, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return 0, err
			}
		}
	}
	if cl < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if _, err := br.Discard(cl); err != nil {
		return 0, err
	}
	return n + cl, nil
}
