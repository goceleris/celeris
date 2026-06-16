//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// fileServeHandler serves a single on-disk file. It mirrors the celeris
// Context.File decision: prefer the zero-copy stream.FileResponder path
// (sendfile) and fall back to a buffered WriteResponse when the responder
// declines (engine not sendfile-capable, or body below threshold). It
// records whether sendfile was actually used so the test can assert the
// threshold gate. This exercises the real wireup: SetSendFileFn hook →
// sendfileState → socket.
type fileServeHandler struct {
	path         string
	contentType  string
	usedSendfile atomic.Bool
	declined     atomic.Bool
}

func (h *fileServeHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	f, err := os.Open(h.path)
	if err != nil {
		return s.ResponseWriter.WriteResponse(s, 500, nil, []byte(err.Error()))
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	if fr, ok := s.ResponseWriter.(stream.FileResponder); ok {
		headers := [][2]string{{"content-type", h.contentType}}
		handled, ferr := fr.WriteFileResponse(s, 200, headers, f, 0, size)
		if handled {
			h.usedSendfile.Store(true)
			return ferr
		}
		h.declined.Store(true)
	}

	// Fallback: buffered read + WriteResponse.
	data := make([]byte, size)
	if rerr := readFullAt(f, data, 0); rerr != nil {
		return rerr
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", h.contentType}}, data)
}

func startFileEngine(t *testing.T, h stream.Handler) *Engine {
	t.Helper()
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
			Workers: 1,
		},
	}
	e, err := New(cfg, h)
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() { cancel(); <-errCh })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.Addr() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	return e
}

func writeFilePattern(t *testing.T, size int) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, fmt.Sprintf("payload_%d.bin", size))
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return path, payload
}

// doHTTPGet performs a raw HTTP/1.1 request over a fresh TCP connection
// and returns the parsed response. Uses net/http's reader on a manual
// conn so we exercise the engine's real socket path (not an in-process
// recorder).
func doHTTPGet(t *testing.T, addr, method, path string) *http.Response {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	req := method + " " + path + " HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("write req: %v", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

// TestSendfileEndToEndLargeFile proves the full wireup: a >1 MiB file
// served via the epoll engine reaches the client byte-exact, and the
// zero-copy sendfile path (not the buffered fallback) was used.
func TestSendfileEndToEndLargeFile(t *testing.T) {
	const size = 2 << 20 // 2 MiB, well above sendfileThreshold
	path, payload := writeFilePattern(t, size)
	h := &fileServeHandler{path: path, contentType: "application/octet-stream"}
	e := startFileEngine(t, h)

	resp := doHTTPGet(t, e.Addr().String(), "GET", "/big")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 0, size)
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	if len(body) != size {
		t.Fatalf("body length = %d, want %d", len(body), size)
	}
	if string(body) != string(payload) {
		// Find first mismatch for a useful message.
		for i := range body {
			if body[i] != payload[i] {
				t.Fatalf("body mismatch at byte %d: got %d want %d", i, body[i], payload[i])
			}
		}
		t.Fatal("body mismatch (length matched, content differed)")
	}
	if !h.usedSendfile.Load() {
		t.Error("expected the sendfile zero-copy path to be used for a 2 MiB file")
	}
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprint(size) {
		t.Errorf("Content-Length = %q, want %d", cl, size)
	}
}

// TestSendfileHEADNoBody pins the HEAD invariant on the sendfile path:
// the response carries the correct Content-Length but ZERO body bytes,
// and the sendfile syscall path must not transfer the file.
func TestSendfileHEADNoBody(t *testing.T) {
	const size = 2 << 20
	path, _ := writeFilePattern(t, size)
	h := &fileServeHandler{path: path, contentType: "application/octet-stream"}
	e := startFileEngine(t, h)

	resp := doHTTPGet(t, e.Addr().String(), "HEAD", "/big")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprint(size) {
		t.Errorf("HEAD Content-Length = %q, want %d", cl, size)
	}
	body := make([]byte, 1)
	n, _ := resp.Body.Read(body)
	if n != 0 {
		t.Errorf("HEAD response had %d body bytes, want 0", n)
	}
	// WriteFileResponse handles HEAD itself (headers only) — the handler's
	// FileResponder branch returns handled=true, so usedSendfile is set,
	// but no body bytes are on the wire (asserted above).
	if h.declined.Load() {
		t.Error("HEAD path unexpectedly declined the FileResponder")
	}
}

// TestSendfileSubThresholdFallback pins that a file below sendfileThreshold
// is NOT served via sendfile — the adapter declines so the handler's
// buffered fallback runs — while still delivering the body byte-exact.
func TestSendfileSubThresholdFallback(t *testing.T) {
	const size = 4 << 10 // 4 KiB, below sendfileThreshold (16 KiB)
	path, payload := writeFilePattern(t, size)
	h := &fileServeHandler{path: path, contentType: "text/plain"}
	e := startFileEngine(t, h)

	resp := doHTTPGet(t, e.Addr().String(), "GET", "/small")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 0, size)
	buf := make([]byte, 8<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	if string(body) != string(payload) {
		t.Errorf("sub-threshold body mismatch (len got=%d want=%d)", len(body), size)
	}
	if h.usedSendfile.Load() {
		t.Error("sub-threshold file should NOT use sendfile (buffered write coalesces better)")
	}
	if !h.declined.Load() {
		t.Error("expected the FileResponder to decline the sub-threshold body")
	}
}

// TestSendfileRangeRequest exercises the 206 partial-content path through
// the engine: the responder is handed (offset, length) for the range and
// must deliver exactly those bytes.
func TestSendfileRangeRequest(t *testing.T) {
	const size = 1 << 20
	path, payload := writeFilePattern(t, size)
	const start, end = 100000, 600000 // length 500001, above threshold
	h := &rangeServeHandler{path: path, start: start, end: end}
	e := startFileEngine(t, h)

	resp := doHTTPGet(t, e.Addr().String(), "GET", "/range")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 206 {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body := make([]byte, 0)
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	want := payload[start : end+1]
	if len(body) != len(want) {
		t.Fatalf("range body length = %d, want %d", len(body), len(want))
	}
	if string(body) != string(want) {
		t.Error("range body content mismatch")
	}
	if cr := resp.Header.Get("Content-Range"); !strings.Contains(cr, "bytes 100000-600000/") {
		t.Errorf("Content-Range = %q", cr)
	}
}

type rangeServeHandler struct {
	path       string
	start, end int64
}

func (h *rangeServeHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	f, err := os.Open(h.path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, _ := f.Stat()
	length := h.end - h.start + 1
	cr := fmt.Sprintf("bytes %d-%d/%d", h.start, h.end, fi.Size())
	fr, ok := s.ResponseWriter.(stream.FileResponder)
	if !ok {
		return s.ResponseWriter.WriteResponse(s, 500, nil, []byte("no FileResponder"))
	}
	headers := [][2]string{
		{"content-type", "application/octet-stream"},
		{"content-range", cr},
	}
	handled, ferr := fr.WriteFileResponse(s, 206, headers, f, h.start, length)
	if !handled {
		// Buffered fallback for the range.
		data := make([]byte, length)
		if rerr := readFullAt(f, data, h.start); rerr != nil {
			return rerr
		}
		return s.ResponseWriter.WriteResponse(s, 206, headers, data)
	}
	return ferr
}
