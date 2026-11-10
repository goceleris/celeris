//go:build linux

package epoll

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// bench757Handler answers every stream with a small JSON body, inline.
type bench757Handler struct{}

func (bench757Handler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := `{"message":"Hello, World!"}`
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/json"}, {"content-length", strconv.Itoa(len(body))}}, []byte(body))
}

// BenchmarkEpollH2ReadWrite757 is the epoll HTTP/2 read, ProcessH2, inline flush
// path end to end (celeris#757 diag): 8 prior-knowledge h2c conns, one stream at
// a time each, one op = one request and its response. It uses nothing the
// diagnostic adds, so the same file runs on main and on the branch: the gate-off
// A/B and the A/A noise floor of the gate-off cost. Not a regression bench of
// the repo.
func BenchmarkEpollH2ReadWrite757(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr: addr, Protocol: engine.H2C, AsyncHandlers: true,
		Resources: resource.Resources{Workers: 2},
	}, bench757Handler{})
	if err != nil {
		b.Fatalf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Listen(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for dl := time.Now().Add(15 * time.Second); (e.Addr() == nil || e.NumWorkers() == 0) && time.Now().Before(dl); {
		time.Sleep(2 * time.Millisecond)
	}
	if e.Addr() == nil {
		b.Fatal("engine did not bind")
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/json"}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	block := hb.Bytes()
	const conns = 8
	type client struct {
		fr   *http2.Framer
		next uint32
	}
	clients := make(chan *client, conns)
	for range conns {
		c, err := net.Dial("tcp", e.Addr().String())
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		fr := http2.NewFramer(c, bufio.NewReader(c))
		_, _ = c.Write([]byte(http2.ClientPreface))
		_ = fr.WriteSettings()
		_ = fr.WriteWindowUpdate(0, 1<<30) // else the server stops after ~65 KiB of responses
		clients <- &client{fr: fr, next: 1}
	}
	var failed atomic.Bool
	b.SetParallelism(1)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		cl := <-clients
		for pb.Next() {
			id := cl.next
			cl.next += 2
			if err := cl.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: block, EndStream: true, EndHeaders: true}); err != nil {
				failed.Store(true)
				return
			}
			for done := false; !done; {
				f, err := cl.fr.ReadFrame()
				if err != nil {
					failed.Store(true)
					return
				}
				switch f := f.(type) {
				case *http2.SettingsFrame:
					if !f.IsAck() {
						_ = cl.fr.WriteSettingsAck()
					}
				case *http2.DataFrame:
					done = f.StreamID == id && f.StreamEnded()
				}
			}
		}
		clients <- cl
	})
	if failed.Load() {
		b.Fatal("a client failed")
	}
}

// BenchmarkEpollDrainReadH2757 is the same path with no scheduler in it: one
// goroutine owns a Loop and one h2c conn on a socketpair, and per op writes a
// request, calls drainRead (read, ProcessH2, inline flush) and reads the
// response. The gate-off A/B of the celeris#757 stamps: the file runs on main
// and on the branch unchanged. "sync" and "async" (cs.detachMu around ProcessH2
// and the flush).
func BenchmarkEpollDrainReadH2757(b *testing.B) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		b.Run(name, func(b *testing.B) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = unix.Close(fds[0]); _ = unix.Close(fds[1]) }()
			srv, cli := fds[0], fds[1]
			l := &Loop{
				conns:        make([]*connState, 64),
				liveConns:    make([]*connState, 0, 4),
				activeConns:  &atomic.Int64{},
				closeCount:   &atomic.Uint64{},
				bytesWritten: &atomic.Uint64{},
				reqCount:     &atomic.Uint64{},
				handler:      bench757Handler{},
				async:        async,
				timerFD:      -1,
				epollFD:      -1,
				cfg:          resource.Config{},
			}
			cs := acquireConnState(context.Background(), srv, 16<<10, async)
			cs.liveIdx = -1
			cs.detected, cs.protocol = true, engine.H2C
			cs.writeFn = l.makeWriteFn(cs)
			cs.h2State = conn.NewH2State(l.handler, sweepCfg889(), cs.writeFn, nil)
			l.conns[srv] = cs
			l.addLiveConn(cs)
			l.h2Conns = append(l.h2Conns, srv)

			var hb bytes.Buffer
			enc := hpack.NewEncoder(&hb)
			enc.SetMaxDynamicTableSize(0) // stateless blocks: the same bytes every request
			field := func() {
				for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/json"}, {":authority", "x"}} {
					_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
				}
			}
			var wire bytes.Buffer
			wire.WriteString(http2.ClientPreface)
			fr := http2.NewFramer(&wire, nil)
			_ = fr.WriteSettings()
			_ = fr.WriteWindowUpdate(0, 1<<30)
			field() // the first block carries the table-size update
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
			hb.Reset()
			field()
			block := append([]byte(nil), hb.Bytes()...)
			if _, err := unix.Write(cli, wire.Bytes()); err != nil {
				b.Fatal(err)
			}
			rbuf := make([]byte, 64<<10)
			now := mono.Now()
			l.drainRead(srv, now)
			for { // the server's SETTINGS and the first response
				if n, err := unix.Read(cli, rbuf); n <= 0 || err != nil {
					break
				}
			}
			req := make([]byte, 9+len(block))
			req[0], req[1], req[2] = 0, 0, byte(len(block))
			req[3], req[4] = 0x1, 0x5 // HEADERS, END_STREAM|END_HEADERS
			copy(req[9:], block)
			id := uint32(3)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req[5], req[6], req[7], req[8] = byte(id>>24), byte(id>>16), byte(id>>8), byte(id)
				id += 2
				if _, err := unix.Write(cli, req); err != nil {
					b.Fatal(err)
				}
				l.drainRead(srv, now)
				if n, err := unix.Read(cli, rbuf); n <= 0 || err != nil {
					b.Fatalf("no response to request %d: n=%d err=%v", i, n, err)
				}
			}
			b.StopTimer()
			if l.conns[srv] != cs {
				b.Fatal("the conn was closed")
			}
		})
	}
}
