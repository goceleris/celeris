//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#870: with the protocol undetected (Protocol Auto, or H2C with the
// RFC 7540 section 3.2 upgrade enabled), the epoll engine read every segment
// at the start of its buffer and went on when detection needed more bytes, so
// the next read overwrote the too-short first one. "GE" then "T /h HTTP/1.1..."
// reached the parser as the method "T" and was answered 405, and an h2c preface
// cut before byte 24 was lost. io_uring keeps those bytes (cs.detectAccum) and
// std reads through net/http, so the test runs on every engine: epoll and
// adaptive (which starts on epoll) must now agree with them.
//
// Each piece goes out as its own TCP segment (TCP_NODELAY) and the next only
// after pieceGap870, long enough for the engine to have read the previous one
// on its own, which is the interleaving the defect needs. Every case has a
// hard floor: the response must arrive, so a case that hangs or answers wrong
// fails, never passes by timing.

const pieceGap870 = 60 * time.Millisecond

const (
	req870     = "GET /h HTTP/1.1\r\nHost: example.com\r\n\r\n"
	preface870 = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

// h2Request870 is, after the client preface, an empty SETTINGS frame and a
// HEADERS frame (END_STREAM|END_HEADERS, stream 1) for GET /h on
// example.com, in HPACK literals so no dynamic table is involved.
func h2Request870() string {
	hdr := []byte{0x82, 0x86, 0x44, 0x02, '/', 'h', 0x41, 0x0b}
	hdr = append(hdr, "example.com"...)
	settings := []byte{0, 0, 0, 0x04, 0, 0, 0, 0, 0}
	headers := []byte{0, 0, byte(len(hdr)), 0x01, 0x05, 0, 0, 0, 1}
	return preface870 + string(settings) + string(headers) + string(hdr)
}

// split870 cuts s at the given offsets (each strictly increasing, inside s).
func split870(s string, at ...int) []string {
	var out []string
	prev := 0
	for _, a := range at {
		out = append(out, s[prev:a])
		prev = a
	}
	return append(out, s[prev:])
}

// startServer870 starts a server on a free port with the routes this test
// needs and returns its address.
func startServer870(t *testing.T, cfg celeris.Config) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		cfg.Addr = addr
		s := celeris.New(cfg)
		s.GET("/h", func(c *celeris.Context) error { return c.String(http.StatusOK, "h1-answer") })
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		s.POST("/len", func(c *celeris.Context) error {
			return c.String(http.StatusOK, "%d", len(c.Body()))
		})
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		err = waitReady761(addr, startDone)
		if err == nil {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				select {
				case <-startDone:
				case <-time.After(15 * time.Second):
					t.Errorf("Start did not return within 15s of Shutdown")
				}
			})
			return addr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start (try %d): %v", tries, err)
	}
}

// sendPieces870 dials addr and writes each piece as its own segment, pieceGap870
// apart. It returns the open connection with a deadline set.
func sendPieces870(t *testing.T, addr string, pieces []string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	for i, p := range pieces {
		if i > 0 {
			time.Sleep(pieceGap870)
		}
		if _, err := io.WriteString(c, p); err != nil {
			t.Fatalf("write piece %d (%d bytes): %v", i, len(p), err)
		}
	}
	return c
}

// engines870 is every engine, each with the Protocol setting that leaves the
// protocol undetected on accept. adaptive is pinned to start on epoll.
var protocols870 = []struct {
	name string
	cfg  func(c celeris.Config) celeris.Config
}{
	{"auto", func(c celeris.Config) celeris.Config { c.Protocol = celeris.Auto; return c }},
	{"h2c-upgrade", func(c celeris.Config) celeris.Config {
		t := true
		c.Protocol = celeris.H2C
		c.EnableH2Upgrade = &t
		return c
	}},
}

func forEngine870(t *testing.T, run func(t *testing.T, addrFor func(cfg celeris.Config) string)) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			if e.eng == celeris.Adaptive {
				t.Setenv("CELERIS_ADAPTIVE_START", "epoll")
			}
			for _, p := range protocols870 {
				t.Run(p.name, func(t *testing.T) {
					run(t, func(cfg celeris.Config) string {
						cfg.Engine = e.eng
						return startServer870(t, p.cfg(cfg))
					})
				})
			}
		})
	}
}

// TestSplitFirstSegmentHTTP1Answered870 splits an HTTP/1.1 request inside the
// bytes protocol detection needs (fewer than 4): it must be answered 200, as
// the same request in one segment is.
func TestSplitFirstSegmentHTTP1Answered870(t *testing.T) {
	cases := []struct {
		name string
		at   []int
	}{
		{"one-segment-control", nil},
		{"G|ET", []int{1}},
		{"GE|T", []int{2}},
		{"GET|space", []int{3}},
		{"G|E|T", []int{1, 2}},
		{"G|E|T-and-more", []int{1, 2, 3}},
	}
	forEngine870(t, func(t *testing.T, addrFor func(celeris.Config) string) {
		addr := addrFor(celeris.Config{})
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := sendPieces870(t, addr, split870(req870, tc.at...))
				br := bufio.NewReader(c)
				// The split request, then a second one on the same conn in
				// one segment: the bytes held for detection must not shift
				// the reads that follow it.
				for i, label := range []string{"split request", "follow-up request"} {
					if i == 1 {
						if _, err := io.WriteString(c, req870); err != nil {
							t.Fatalf("write follow-up: %v", err)
						}
					}
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						t.Fatalf("celeris870: no response to the %s: %v", label, err)
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusOK || string(body) != "h1-answer" {
						t.Fatalf("celeris870: %s (%s) answered %d %q, want 200 %q",
							label, tc.name, resp.StatusCode, body, "h1-answer")
					}
				}
			})
		}
	})
}

// frame870 is one HTTP/2 frame read off the wire.
type frame870 struct {
	typ, flags byte
	stream     uint32
	payload    []byte
}

func readFrame870(r io.Reader) (frame870, error) {
	var h [9]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return frame870{}, err
	}
	n := int(h[0])<<16 | int(h[1])<<8 | int(h[2])
	f := frame870{typ: h[3], flags: h[4], stream: binary.BigEndian.Uint32(h[5:]) & 0x7fffffff, payload: make([]byte, n)}
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return frame870{}, err
	}
	return f, nil
}

// TestSplitH2CPrefaceAnswered870 cuts the HTTP/2 client preface before its
// 24th byte. The conn must still be recognised as h2c and answer stream 1: a
// SETTINGS frame of its own, then HEADERS and DATA "h1-answer".
func TestSplitH2CPrefaceAnswered870(t *testing.T) {
	cases := []struct {
		name string
		at   []int
	}{
		{"one-segment-control", nil},
		{"3|rest", []int{3}},
		{"4|rest", []int{4}},
		{"10|rest", []int{10}},
		{"23|rest", []int{23}},
		{"10|13|rest", []int{10, 13}},
		{"4|10|23|rest", []int{4, 10, 23}},
	}
	forEngine870(t, func(t *testing.T, addrFor func(celeris.Config) string) {
		addr := addrFor(celeris.Config{})
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := sendPieces870(t, addr, split870(h2Request870(), tc.at...))
				br := bufio.NewReader(c)
				sawSettings, sawHeaders := false, false
				var data []byte
				for {
					f, err := readFrame870(br)
					if err != nil {
						t.Fatalf("celeris870: read frame (settings=%v headers=%v data=%q): %v",
							sawSettings, sawHeaders, data, err)
					}
					switch {
					case f.typ == 0x4 && f.flags&0x1 == 0:
						sawSettings = true
					case f.typ == 0x1 && f.stream == 1:
						sawHeaders = true
					case f.typ == 0x0 && f.stream == 1:
						data = append(data, f.payload...)
					case f.typ == 0x7:
						t.Fatalf("celeris870: GOAWAY after the split preface: % x", f.payload)
					}
					if f.stream == 1 && f.flags&0x1 != 0 && (f.typ == 0x0 || f.typ == 0x1) {
						break
					}
				}
				if !sawSettings || !sawHeaders || string(data) != "h1-answer" {
					t.Fatalf("celeris870: split preface answered settings=%v headers=%v data=%q, want true true %q",
						sawSettings, sawHeaders, data, "h1-answer")
				}
			})
		}
	})
}

// TestSplitFirstSegmentThenReadFillingBuffer870 pins the read after the held
// bytes: "GE" is held at the head of the connection's buffer, so the next read
// has less room than a whole buffer. A body several buffers long arrives in
// that read in one segment; reading it must go on until the socket is drained
// (a short-read test against the whole buffer length stops one read early and,
// the socket being edge-triggered, never sees the rest).
func TestSplitFirstSegmentThenReadFillingBuffer870(t *testing.T) {
	const bufSize = 4096 // resource.MinBufferSize
	body := strings.Repeat("x", 3*bufSize)
	req := "POST /len HTTP/1.1\r\nHost: example.com\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	forEngine870(t, func(t *testing.T, addrFor func(celeris.Config) string) {
		addr := addrFor(celeris.Config{BufferSize: bufSize})
		c := sendPieces870(t, addr, split870(req, 2))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("celeris870: no response to the split POST: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		got, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(got) != fmt.Sprint(len(body)) {
			t.Fatalf("celeris870: split POST answered %d %q, want 200 %d", resp.StatusCode, got, len(body))
		}
	})
}

// TestSplitFirstSegmentAsync870 runs the split shapes with AsyncHandlers on.
// In async mode the bytes the engine read reach the dispatch goroutine through
// a different path (the conn's asyncInBuf, not the loop's parse), so the held
// bytes have to arrive there whole too. Only epoll and adaptive (pinned to
// epoll) have that path; std and io_uring are covered above.
func TestSplitFirstSegmentAsync870(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"adaptive", celeris.Adaptive}} {
		t.Run(e.name, func(t *testing.T) {
			if e.eng == celeris.Adaptive {
				t.Setenv("CELERIS_ADAPTIVE_START", "epoll")
			}
			for _, p := range protocols870 {
				t.Run(p.name, func(t *testing.T) {
					addr := startServer870(t, p.cfg(celeris.Config{Engine: e.eng, AsyncHandlers: true}))
					for _, at := range [][]int{{2}, {1, 2}, {3}} {
						t.Run(fmt.Sprintf("h1-%v", at), func(t *testing.T) {
							c := sendPieces870(t, addr, split870(req870, at...))
							br := bufio.NewReader(c)
							// The split request, then a follow-up on the same conn.
							for i, label := range []string{"split request", "follow-up request"} {
								if i == 1 {
									if _, err := io.WriteString(c, req870); err != nil {
										t.Fatalf("write follow-up: %v", err)
									}
								}
								resp, err := http.ReadResponse(br, nil)
								if err != nil {
									t.Fatalf("celeris870: async: no response to the %s: %v", label, err)
								}
								body, _ := io.ReadAll(resp.Body)
								_ = resp.Body.Close()
								if resp.StatusCode != http.StatusOK || string(body) != "h1-answer" {
									t.Fatalf("celeris870: async: %s answered %d %q, want 200 %q",
										label, resp.StatusCode, body, "h1-answer")
								}
							}
						})
					}
					for _, at := range [][]int{{3}, {10, 13}, {23}} {
						t.Run(fmt.Sprintf("h2c-%v", at), func(t *testing.T) {
							c := sendPieces870(t, addr, split870(h2Request870(), at...))
							br := bufio.NewReader(c)
							var data []byte
							for {
								f, err := readFrame870(br)
								if err != nil {
									t.Fatalf("celeris870: async: read frame (data=%q): %v", data, err)
								}
								if f.typ == 0x7 {
									t.Fatalf("celeris870: async: GOAWAY after the split preface: % x", f.payload)
								}
								if f.typ == 0x0 && f.stream == 1 {
									data = append(data, f.payload...)
								}
								if f.stream == 1 && f.flags&0x1 != 0 && (f.typ == 0x0 || f.typ == 0x1) {
									break
								}
							}
							if string(data) != "h1-answer" {
								t.Fatalf("celeris870: async: split preface answered data %q, want %q", data, "h1-answer")
							}
						})
					}
				})
			}
		})
	}
}
