//go:build linux

package epoll

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 items 35 and 45, end to end: a client that takes a large keep-alive
// response steadily is never cut by WriteTimeout, WHATEVER ELSE IT SENDS while
// it reads. WriteTimeout is "no progress for WriteTimeout" (writeSince,
// restarted when a flush takes bytes). A client that sends while it reads (an
// HTTP/2 PING, WINDOW_UPDATE or another request on the conn; an HTTP/1.1
// pipelined request) raises an EPOLLIN event per segment, and drainRead's
// inline flush takes the room the client made: its partial branch parks the
// conn again (armEpollOut) and must note the progress, or the clock runs from
// the response's first partial flush and cuts the client at WriteTimeout while
// it is still taking the response. (On main the response was never bounded by
// WriteTimeout at all, so no client was cut.) WriteTimeout 1 s, ReadTimeout and
// IdleTimeout 30 s: the only thing that can cut the response is the write
// clock.
//
// There is no arm for an HTTP/2 handler that runs on the loop thread: one
// WriteResponse of this size delivers nothing to the client on main either
// (only the server preface arrives), so it says nothing about the clock.

// streamHandler885 answers every HTTP/2 stream with 200 and big, written in 1 MiB
// chunks, and marks every route async, as the engine's async handlers do, so
// the handler runs off the loop thread.
type streamHandler885 struct{ big []byte }

func (h *streamHandler885) HandleStream(_ context.Context, s *stream.Stream) error {
	st, ok := s.ResponseWriter.(stream.Streamer)
	if !ok {
		return nil
	}
	if err := st.WriteHeader(s, 200, [][2]string{{"content-type", "application/octet-stream"}}); err != nil {
		return nil
	}
	for off := 0; off < len(h.big); off += 1 << 20 {
		if err := st.Write(s, h.big[off:min(off+1<<20, len(h.big))]); err != nil {
			return nil
		}
	}
	return nil
}
func (*streamHandler885) RouteAsync(string, string) bool { return true }
func (*streamHandler885) HasAsyncRoutes() bool           { return true }

var _ stream.AsyncRouteResolver = (*streamHandler885)(nil)

// readPaced885 reads c at about rate bytes a second, in 32 KiB reads, until it
// has want bytes, the conn ends, or limit passes; it reports what it got and
// when the conn ended (zero if it did not).
func readPaced885(c net.Conn, want, rate int, limit time.Duration, stop *atomic.Bool) (got int, ended time.Duration, endErr error) {
	buf := make([]byte, 32<<10)
	start := time.Now()
	per := time.Duration(float64(time.Second) * float64(len(buf)) / float64(rate))
	defer stop.Store(true)
	for got < want && time.Since(start) < limit {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.Read(buf)
		got += n
		if err != nil {
			return got, time.Since(start), err
		}
		time.Sleep(per)
	}
	return got, 0, nil
}

func TestEngineSteadyReaderThatSendsWhileReadingIsNotCutByWriteTimeout885(t *testing.T) {
	// big: 24 MiB at 6 MiB/s on the default (autotuned) send buffer. small: 3.5
	// MiB at 1 MiB/s with SocketSend 64 KiB, so that an HTTP/1 queue stays
	// under the 4 MiB backlog cap (overBacklogH1) and the pipelined requests
	// are served.
	const bigSize, bigRate, smallSize, smallRate = 24 << 20, 6 << 20, 7 << 19, 1 << 20
	for _, tc := range []struct {
		name  string
		h2    bool
		async bool // the engine's AsyncHandlers (an HTTP/2 handler is always streamHandler885's, which runs off the loop thread either way)
		small bool
		every time.Duration // 0: the client sends nothing while it reads
	}{
		{"h1/sync/silent", false, false, true, 0},
		{"h1/sync/pipelined-every-5ms", false, false, true, 5 * time.Millisecond},
		{"h1/async/pipelined-every-5ms", false, true, true, 5 * time.Millisecond},
		{"h2/silent", true, false, false, 0},
		{"h2/ping-every-20ms", true, false, false, 20 * time.Millisecond},
		{"h2/asynchandlers/ping-every-20ms", true, true, false, 20 * time.Millisecond},
		{"h2/smallsnd/ping-every-5ms", true, false, true, 5 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size, rate, sndbuf := bigSize, bigRate, 0
			if tc.small {
				size, rate, sndbuf = smallSize, smallRate, 64<<10
			}
			var h stream.Handler = newPathHandler885(size)
			if tc.h2 {
				h = &streamHandler885{big: bytes.Repeat([]byte{'X'}, size)}
			}
			_, addr := startE2E885(t, tc.async, h, func(c *resource.Config) {
				if tc.h2 {
					c.Protocol = engine.H2C
				}
				c.WriteTimeout = time.Second
				c.ReadTimeout = 30 * time.Second
				c.IdleTimeout = 30 * time.Second
				c.Resources.SocketSend = sndbuf
			})
			c := dialRcvBuf885(t, addr, 64<<10)
			var fr *http2.Framer
			if tc.h2 {
				if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
					t.Fatal(err)
				}
				fr = http2.NewFramer(c, nil)
				if err := fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1<<31 - 1}); err != nil {
					t.Fatal(err)
				}
				if err := fr.WriteWindowUpdate(0, 1<<31-1-65535); err != nil {
					t.Fatal(err)
				}
				var hb bytes.Buffer
				enc := hpack.NewEncoder(&hb)
				for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/big"}, {":authority", "x"}} {
					_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
				}
				if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
					t.Fatal(err)
				}
			} else {
				get885(t, c, "/big")
			}
			var stop atomic.Bool
			var sent atomic.Int64
			if tc.every > 0 {
				go func() {
					for seq := byte(1); !stop.Load(); seq++ {
						time.Sleep(tc.every)
						if stop.Load() {
							return
						}
						var err error
						if tc.h2 {
							err = fr.WritePing(false, [8]byte{7: seq})
						} else {
							_, err = fmt.Fprintf(c, "GET /small HTTP/1.1\r\nHost: x\r\n\r\n")
						}
						if err != nil {
							return
						}
						sent.Add(1)
					}
				}()
			}
			got, ended, err := readPaced885(c, size, rate, 20*time.Second, &stop)
			t.Logf("%s: received %d of at least %d bytes; the client sent %d PINGs/requests while reading; ended at %v (%v)",
				tc.name, got, size, sent.Load(), ended.Round(time.Millisecond), err)
			if got < size {
				t.Errorf("celeris885 items 35 and 45: a client reading steadily (%d B/s) and sending every %v was cut after %v with %d of %d bytes "+
					"(WriteTimeout 1 s, ReadTimeout and IdleTimeout 30 s): a flush that took bytes did not restart the write clock", rate, tc.every, ended.Round(time.Millisecond), got, size)
			}
		})
	}
}
