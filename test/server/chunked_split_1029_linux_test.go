//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#1029: on epoll, io_uring and adaptive a chunked request body that
// did not arrive in one recv was answered 400. This drives all four engines
// with a raw TCP client that controls where the segments end: bodies of 1 B,
// 4 KiB, 64 KiB, 1 MiB and 4 MiB (served) and 4 MiB + 1 (413, the default
// limit), each delivered in 1, 2 and many segments, and the structural cuts of
// the wire format (inside the size line, inside the CRLF, inside chunk data,
// in the trailer), then the connection reused. The exact recv boundaries are
// the kernel's; internal/conn/h1_chunked_split_1029_test.go fixes them at the
// ProcessH1 level.

func pat1029(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i*7+i>>9)%26)
	}
	return b
}

// wire1029 is one chunked request: head, chunks of size chunk with ext, the
// terminating chunk and trailer. It returns the bytes and the offsets of the
// structural points worth cutting at.
func wire1029(path string, body []byte, chunk int, ext, trailer, extraHead string) (wire []byte, cuts map[string][]int) {
	var b bytes.Buffer
	b.WriteString("POST " + path + " HTTP/1.1\r\nHost: a\r\n" + extraHead + "Transfer-Encoding: chunked\r\n\r\n")
	headEnd := b.Len()
	cuts = map[string][]int{"head-alone": {headEnd}}
	first := true
	for off := 0; off < len(body); off += chunk {
		end := min(off+chunk, len(body))
		start := b.Len()
		line := fmt.Sprintf("%x%s\r\n", end-off, ext)
		b.WriteString(line)
		if first {
			cuts["inside-size-line"] = []int{start + 1}
			cuts["between-CR-LF-of-size"] = []int{start + len(line) - 1}
			cuts["after-size-line"] = []int{start + len(line)}
			cuts["inside-data"] = []int{start + len(line) + (end-off)/2}
		}
		b.Write(body[off:end])
		if first {
			cuts["data-end-before-CRLF"] = []int{b.Len()}
			cuts["between-CR-LF-of-data"] = []int{b.Len() + 1}
		}
		b.WriteString("\r\n")
		first = false
	}
	term := b.Len()
	b.WriteString("0\r\n")
	cuts["after-terminal-size-line"] = []int{term + 3}
	if trailer != "" {
		cuts["inside-trailer"] = []int{term + 3 + 2}
	}
	b.WriteString(trailer)
	cuts["before-final-CRLF"] = []int{b.Len()}
	b.WriteString("\r\n")
	cuts["between-final-CR-LF"] = []int{b.Len() - 1}
	return b.Bytes(), cuts
}

type seg1029 struct {
	b   []byte
	gap time.Duration
}

func plan1029(wire []byte, offs []int, gap time.Duration) []seg1029 {
	var out []seg1029
	prev := 0
	for _, o := range offs {
		if o <= prev || o >= len(wire) {
			continue
		}
		out = append(out, seg1029{wire[prev:o], gap})
		prev = o
	}
	return append(out, seg1029{wire[prev:], 0})
}

func equal1029(wire []byte, n int, gap time.Duration) []seg1029 {
	var offs []int
	for i := 1; i < n; i++ {
		offs = append(offs, len(wire)*i/n)
	}
	return plan1029(wire, offs, gap)
}

// send1029 writes plan to a fresh connection from a goroutine (a server that
// refuses early stops reading, which is not the writer's concern) and returns
// the reader of the responses.
func send1029(t *testing.T, addr string, plan []seg1029) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	go write1029(c, plan)
	return c, bufio.NewReaderSize(c, 64<<10)
}

func write1029(c net.Conn, plan []seg1029) {
	for _, s := range plan {
		if _, err := c.Write(s.b); err != nil {
			return
		}
		if s.gap > 0 {
			time.Sleep(s.gap)
		}
	}
}

func read1029(t *testing.T, br *bufio.Reader) (status int, body string) {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return 0, "read error: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode, string(b)
}

func TestChunkedBodySplitAcrossRecvs1029(t *testing.T) {
	const miB = 1 << 20
	sizes := []struct {
		name string
		n    int
		ok   bool
	}{
		{"1B", 1, true}, {"4KiB", 4 << 10, true}, {"64KiB", 64 << 10, true}, {"1MiB", miB, true},
		{"4MiB", 4 * miB, true}, {"4MiB+1", 4*miB + 1, false},
	}
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := bodyServer889(t, e.eng, 0) // the default limit: 4 MiB
			for _, sz := range sizes {
				body := pat1029(sz.n)
				for _, shape := range []struct {
					name  string
					chunk int
				}{{"one-chunk", sz.n}, {"64KiB-chunks", 64 << 10}, {"1B-chunks", 1}} {
					if shape.name == "1B-chunks" && sz.n > 4<<10 {
						continue
					}
					if shape.name == "64KiB-chunks" && sz.n <= 64<<10 {
						continue
					}
					wire, _ := wire1029("/up", body, shape.chunk, "", "", "")
					for _, mode := range []struct {
						name string
						plan []seg1029
					}{
						{"1-segment", []seg1029{{wire, 0}}},
						{"2-segments", equal1029(wire, 2, 5*time.Millisecond)},
						{"many-segments", equal1029(wire, 24, time.Millisecond)},
					} {
						t.Run(sz.name+"/"+shape.name+"/"+mode.name, func(t *testing.T) {
							_, br := send1029(t, addr, mode.plan)
							status, got := read1029(t, br)
							if sz.ok {
								if status != 200 || got != fmt.Sprintf("n=%d", sz.n) {
									t.Fatalf("status %d body %q, want 200 n=%d", status, got, sz.n)
								}
								return
							}
							if status != 413 {
								t.Fatalf("status %d (%q), want 413", status, got)
							}
						})
					}
				}
			}
		})
	}
}

func TestChunkedBodyStructuralCuts1029(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := bodyServer889(t, e.eng, 0)
			body := pat1029(5000)
			wire, cuts := wire1029("/up", body, 2000, ";ext=1", "X-Trailer: 1\r\n", "")
			for name, offs := range cuts {
				t.Run(name, func(t *testing.T) {
					_, br := send1029(t, addr, plan1029(wire, offs, 8*time.Millisecond))
					status, got := read1029(t, br)
					if status != 200 || got != "n=5000" {
						t.Fatalf("status %d body %q, want 200 n=5000", status, got)
					}
				})
			}
			t.Run("every-cut-at-once", func(t *testing.T) {
				var all []int
				for _, offs := range cuts {
					all = append(all, offs...)
				}
				slices.Sort(all)
				_, br := send1029(t, addr, plan1029(wire, all, 3*time.Millisecond))
				status, got := read1029(t, br)
				if status != 200 || got != "n=5000" {
					t.Fatalf("status %d body %q, want 200 n=5000", status, got)
				}
			})
		})
	}
}

// TestChunkedKeepAliveAfterSplit1029: a split chunked upload, then a GET and a
// second split upload on the same connection (on the native engines the
// connection was handed to the dispatch goroutine in the middle of the first).
func TestChunkedKeepAliveAfterSplit1029(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := bodyServer889(t, e.eng, 0)
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			br := bufio.NewReader(c)
			for round, n := range []int{70000, 3000, 200000} {
				wire, _ := wire1029("/up", pat1029(n), 9000, "", "", "")
				go write1029(c, equal1029(wire, 6, 3*time.Millisecond))
				if status, got := read1029(t, br); status != 200 || got != fmt.Sprintf("n=%d", n) {
					t.Fatalf("round %d: status %d body %q, want 200 n=%d", round, status, got, n)
				}
				if _, err := io.WriteString(c, "GET /ping HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				if status, got := read1029(t, br); status != 200 || got != "ok" {
					t.Fatalf("round %d: GET after the upload: status %d body %q", round, status, got)
				}
			}
			// a request pipelined behind the terminator, in the segment that ends the body
			wire, _ := wire1029("/up", pat1029(40000), 9000, "", "", "")
			wire = append(wire, "GET /ping HTTP/1.1\r\nHost: a\r\n\r\n"...)
			go write1029(c, equal1029(wire, 5, 3*time.Millisecond))
			if status, got := read1029(t, br); status != 200 || got != "n=40000" {
				t.Fatalf("pipelined: status %d body %q", status, got)
			}
			if status, got := read1029(t, br); status != 200 || got != "ok" {
				t.Fatalf("pipelined GET: status %d body %q", status, got)
			}
		})
	}
}

// TestChunkedNetHTTPClient1029: Go's own client, an unknown-length body, which
// is what the issue reproduced with.
func TestChunkedNetHTTPClient1029(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := bodyServer889(t, e.eng, 0)
			for _, n := range []int64{1 << 10, 64 << 10, 1 << 20, 4 << 20} {
				req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/up", io.NopCloser(&genBody889{left: n}))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("n=%d: %v", n, err)
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != 200 || string(b) != fmt.Sprintf("n=%d", n) {
					t.Fatalf("n=%d: %s %q", n, resp.Status, b)
				}
			}
		})
	}
}

// TestChunkedOversizeChunkRefusedEarly1029: one chunk whose declared size is
// over the limit is refused at its size line, before its data is sent.
func TestChunkedOversizeChunkRefusedEarly1029(t *testing.T) {
	for _, e := range engines761 {
		if e.name == "std" {
			// net/http streams the body to the handler and the limit is a
			// MaxBytesReader on it: nothing is refused before data arrives.
			continue
		}
		t.Run(e.name, func(t *testing.T) {
			addr := bodyServer889(t, e.eng, 64<<10)
			for _, line := range []string{"10001", "ffffffff", "7fffffffffffffff"} {
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				// no data follows the size line: only an early refusal can answer
				_, _ = io.WriteString(c, "POST /up HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n"+line+"\r\n")
				status, got := read1029(t, bufio.NewReader(c))
				_ = c.Close()
				if status != 413 {
					t.Fatalf("size line %q: status %d (%q), want 413 before any data", line, status, got)
				}
			}
		})
	}
}

// TestChunkedExpectContinueSplit1029: one 100 Continue for a chunked body that
// arrives in pieces, then the answer.
func TestChunkedExpectContinueSplit1029(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := bodyServer889(t, e.eng, 0)
			wire, _ := wire1029("/up", pat1029(30000), 7000, "", "", "Expect: 100-continue\r\nConnection: close\r\n")
			c, br := send1029(t, addr, equal1029(wire, 8, 3*time.Millisecond))
			_ = c
			all, _ := io.ReadAll(br)
			s := string(all)
			if n := strings.Count(s, "100 Continue"); n != 1 || !strings.Contains(s, "HTTP/1.1 200") || !strings.Contains(s, "n=30000") {
				t.Fatalf("%d 100 Continue; answer %q, want one 100 then 200 n=30000", n, s)
			}
		})
	}
}

// TestChunkedSlowUploadSurvivesReadHeaderTimeout1029: ReadHeaderTimeout bounds
// the head, not the body. A chunked upload that trickles in for longer than the
// timeout is served.
func TestChunkedSlowUploadSurvivesReadHeaderTimeout1029(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := bodyServerRHT1029(t, e.eng, 300*time.Millisecond)
			wire, _ := wire1029("/up", pat1029(6000), 1000, "", "", "")
			c, br := send1029(t, addr, equal1029(wire, 12, 100*time.Millisecond)) // about 1.1 s in all
			_ = c
			if status, got := read1029(t, br); status != 200 || got != "n=6000" {
				t.Fatalf("status %d body %q, want 200 n=6000", status, got)
			}
		})
	}
}

func bodyServerRHT1029(t *testing.T, eng celeris.EngineType, rht time.Duration) string {
	t.Helper()
	return server1029(t, eng, celeris.Config{ReadHeaderTimeout: rht})
}

// server1029 serves /ping, a sync POST /up and an Async() POST /aup, both
// answering with the body length. It starts through startServerConfig761, which
// retries an io_uring start that fails with ENOMEM: the ring is charged to the
// runner's 8 MiB RLIMIT_MEMLOCK, which another package's test binary running
// at the same time may hold (a start that did not retry failed the Unit steps
// job of celeris#1077 twice with "io_uring_setup: cannot allocate memory").
func server1029(t *testing.T, eng celeris.EngineType, cfg celeris.Config) string {
	t.Helper()
	cfg.Engine, cfg.Protocol = eng, celeris.Auto
	return startServerConfig761(t, cfg, func(s *celeris.Server) {
		up := func(c *celeris.Context) error { return c.String(http.StatusOK, "n=%d", len(c.Body())) }
		s.POST("/up", up)
		s.POST("/aup", up).Async()
	})
}

// TestChunkedAsyncRoute1029: the same split bodies to a route marked Async(),
// whose requests run on the connection's dispatch goroutine, and a connection
// that alternates a sync and an async route.
func TestChunkedAsyncRoute1029(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := server1029(t, e.eng, celeris.Config{})
			for _, n := range []int{1, 3000, 200000, 1 << 20} {
				wire, _ := wire1029("/aup", pat1029(n), 32<<10, "", "", "")
				for _, mode := range []struct {
					name string
					plan []seg1029
				}{
					{"1-segment", []seg1029{{wire, 0}}},
					{"2-segments", equal1029(wire, 2, 5*time.Millisecond)},
					{"many-segments", equal1029(wire, 20, time.Millisecond)},
				} {
					t.Run(fmt.Sprintf("%dB/%s", n, mode.name), func(t *testing.T) {
						_, br := send1029(t, addr, mode.plan)
						if status, got := read1029(t, br); status != 200 || got != fmt.Sprintf("n=%d", n) {
							t.Fatalf("status %d body %q, want 200 n=%d", status, got, n)
						}
					})
				}
			}
			t.Run("alternating", func(t *testing.T) {
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				br := bufio.NewReader(c)
				for round, path := range []string{"/up", "/aup", "/up", "/aup", "/aup", "/up"} {
					n := 5000 + 17000*round
					wire, _ := wire1029(path, pat1029(n), 6000, "", "", "")
					go write1029(c, equal1029(wire, 5, 3*time.Millisecond))
					if status, got := read1029(t, br); status != 200 || got != fmt.Sprintf("n=%d", n) {
						t.Fatalf("round %d %s: status %d body %q, want 200 n=%d", round, path, status, got, n)
					}
				}
			})
		})
	}
}
