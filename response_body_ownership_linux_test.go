//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// Tests for celeris#817. The H1 response adapter hands a body of 8 KiB or
// more to epoll and io_uring through their zero-copy body writer, which kept
// a reference to the caller's slice past the write: epoll sent it at the
// flush after the handler (or later, on EPOLLOUT), io_uring when the ring was
// next entered. The body belongs to the handler, which may reuse it as soon
// as its write returns: c.JSON puts its encode buffer back in a pool at
// once, where the next handler to encode takes it, and a buffered response's
// body lives on a Context that is reused. So a response went out carrying
// the next pipelined request's body, or another connection's. std copies
// every body and never did.

// ownedList761 is a []string (the reflection-free c.JSON fast path, whose
// buffer comes from a pool and goes back when c.JSON returns) of at least
// size bytes of JSON, every 256-byte element naming id and its index, so a
// body that is not its own does not compare equal.
func ownedList761(id, size int) []string {
	tag := strconv.Itoa(10000000 + id)
	fill := strings.Repeat(tag, 256/len(tag))
	var out []string
	for n, i := 0, 0; n < size; i++ {
		s := strconv.Itoa(1000+i) + "-" + fill
		out = append(out, s)
		n += len(s) + 3
	}
	return out
}

type ownedStruct761 struct {
	ID    int      `json:"id"`
	Items []string `json:"items"`
}

// ownedBody761 is the response the /j route sends for id in the given
// kind: "fast" (c.JSON of a []string), "encjson" (c.JSON of a struct, the
// encoding/json path and its pooled encoder) or "buffered" (the same through
// BufferResponse and FlushResponse, whose body is the Context's own).
func ownedBody761(kind string, id, size int) any {
	if kind == "encjson" {
		return ownedStruct761{ID: id, Items: ownedList761(id, size)}
	}
	return ownedList761(id, size)
}

func ownedRoute761(s *celeris.Server, kind string, size int, async bool) {
	r := s.GET("/j", func(c *celeris.Context) error {
		id, err := strconv.Atoi(c.Query("id"))
		if err != nil {
			return err
		}
		if kind == "buffered" {
			c.BufferResponse()
			if err := c.JSON(http.StatusOK, ownedBody761(kind, id, size)); err != nil {
				return err
			}
			return c.FlushResponse()
		}
		return c.JSON(http.StatusOK, ownedBody761(kind, id, size))
	})
	if async {
		r.Async()
	}
}

// firstDiff761 describes where got departs from want.
func firstDiff761(got, want []byte) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	end := min(i+32, len(got))
	return fmt.Sprintf("%d bytes (want %d), differ from byte %d: %q", len(got), len(want), i, got[i:end])
}

// TestPipelinedResponsesOwnTheirBodies pipelines three requests, whose
// responses are 16 KiB c.JSON bodies, in one packet, rounds times on new
// connections, and compares every body with its own. The handler of request
// 2 took the pool buffer request 1's body still pointed into.
func TestPipelinedResponsesOwnTheirBodies(t *testing.T) {
	const (
		size   = 16 << 10
		rounds = 10
	)
	for _, e := range engines761 {
		for _, kind := range []string{"fast", "encjson", "buffered"} {
			t.Run(e.name+"/"+kind, func(t *testing.T) {
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					ownedRoute761(s, kind, size, false)
				})
				bad := 0
				for r := range rounds {
					ids := []int{r*10 + 1, r*10 + 2, r*10 + 3}
					raw, err := net.Dial("tcp", addr)
					if err != nil {
						t.Fatal(err)
					}
					var batch bytes.Buffer
					for _, id := range ids {
						fmt.Fprintf(&batch, "GET /j?id=%d HTTP/1.1\r\nHost: x\r\n\r\n", id)
					}
					if _, err := raw.Write(batch.Bytes()); err != nil {
						t.Fatal(err)
					}
					br := bufio.NewReaderSize(idleConn761{raw}, 64<<10)
					for i, id := range ids {
						want, _ := json.Marshal(ownedBody761(kind, id, size))
						resp, err := http.ReadResponse(br, nil)
						if err != nil {
							t.Errorf("%s/%s round %d: response %d: %s", e.name, kind, r, i+1, describeReadEnd761(err))
							bad++
							break
						}
						got, err := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if err != nil || !bytes.Equal(got, want) {
							t.Errorf("%s/%s round %d: response %d (id %d): %s", e.name, kind, r, i+1, id, firstDiff761(got, want))
							bad++
							break
						}
					}
					_ = raw.Close()
				}
				if bad > 0 {
					t.Errorf("%s/%s: %d of %d rounds had a response that was not its own", e.name, kind, bad, rounds)
				}
			})
		}
	}
}

// TestConcurrentResponsesOwnTheirBodies: conns connections at once, each
// asking rounds times for its own 16 KiB c.JSON body, while the other
// connections' handlers run and take pool buffers; every JSON body is
// compared with its own. "direct" asks for the JSON body alone and reads it
// at once: io_uring's kernel read the body at the next submit, after other
// handlers in the same completion batch had run. "behind-a-large-body"
// pipelines it behind a 2 MiB static body and reads nothing for 100 ms,
// through a 64 KiB receive buffer, so the JSON body waits for the socket:
// epoll left it staged until the socket took it. The other request on a
// connection has no id, so a body carrying another id came from another
// connection's handler.
func TestConcurrentResponsesOwnTheirBodies(t *testing.T) {
	const (
		size  = 16 << 10
		conns = 16
	)
	rounds := 4
	if lean761() {
		rounds = 2
	}
	fill := bytes.Repeat([]byte("f"), 2<<20)
	for _, e := range engines761 {
		for _, mode := range []string{"direct", "behind-a-large-body"} {
			t.Run(e.name+"/"+mode, func(t *testing.T) {
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					ownedRoute761(s, "fast", size, false)
					s.GET("/fill", func(c *celeris.Context) error {
						return c.Blob(http.StatusOK, "application/octet-stream", fill)
					})
				})
				var bad, foreign, total, errs atomic.Int64
				var first atomic.Value
				var wg sync.WaitGroup
				for k := range conns {
					wg.Go(func() {
						raw, err := net.Dial("tcp", addr)
						if err != nil {
							errs.Add(1)
							return
						}
						defer func() { _ = raw.Close() }()
						responses := 1
						if mode == "behind-a-large-body" {
							responses = 2
							_ = raw.(*net.TCPConn).SetReadBuffer(64 << 10)
						}
						br := bufio.NewReaderSize(idleConn761{raw}, 64<<10)
						for r := range rounds {
							id := (k+1)*100000 + r
							req := fmt.Sprintf("GET /j?id=%d HTTP/1.1\r\nHost: x\r\n\r\n", id)
							if responses == 2 {
								req = "GET /fill HTTP/1.1\r\nHost: x\r\n\r\n" + req
							}
							if _, err := io.WriteString(raw, req); err != nil {
								errs.Add(1)
								return
							}
							if responses == 2 {
								time.Sleep(100 * time.Millisecond)
							}
							var got []byte
							for i := range responses {
								resp, err := http.ReadResponse(br, nil)
								if err != nil {
									errs.Add(1)
									first.CompareAndSwap(nil, fmt.Sprintf("id %d, response %d: %s", id, i+1, describeReadEnd761(err)))
									return
								}
								got, err = io.ReadAll(resp.Body)
								_ = resp.Body.Close()
								if err != nil {
									errs.Add(1)
									return
								}
							}
							total.Add(1)
							want, _ := json.Marshal(ownedList761(id, size))
							if !bytes.Equal(got, want) {
								bad.Add(1)
								carried := ""
								if len(got) >= 15 && got[0] == '[' {
									if n, err := strconv.Atoi(string(got[7:15])); err == nil && n-10000000 != id {
										carried = fmt.Sprintf(", carrying id %d", n-10000000)
										if (n-10000000)/100000 != k+1 {
											foreign.Add(1)
										}
									}
								}
								first.CompareAndSwap(nil, fmt.Sprintf("id %d: %s%s", id, firstDiff761(got, want), carried))
							}
						}
					})
				}
				wg.Wait()
				f, _ := first.Load().(string)
				if bad.Load() > 0 || errs.Load() > 0 {
					t.Errorf("%s/%s: %d of %d JSON bodies were not their own (%d carrying another connection's id), %d connections failed; first: %s",
						e.name, mode, bad.Load(), total.Load(), foreign.Load(), errs.Load(), f)
				}
			})
		}
	}
}
