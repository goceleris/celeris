//go:build linux

package epoll

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#876, h2c, through the epoll engine. An h2c conn whose peer stops
// reading after advertising a huge window: an async-route handler bursts past
// maxPendingBytesH2, so a write is refused and closeWhenFlushed defers the
// close, then keeps writing 1 KiB every 50 ms (an SSE stream). The peer takes
// nothing after the burst, so the conn must be reaped about WriteTimeout (1 s)
// after the refusal, however many frames the handler goes on queueing: the
// h2Conns pass re-enters closeWhenFlushed for every one, and each re-entry
// must not restart the drain clock (a restart without progress made the conn
// immortal for as long as the handler wrote). Before the fix of that restamp
// this conn lived 13 s; ReadTimeout is 4 s and the trickle runs for 6 s, so a
// conn still open 3 s after the burst fails it whichever clock held it.
func TestEngineH2ClosingConnIsReapedWhileTheHandlerKeepsWriting(t *testing.T) {
	const trickle = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var burstDone, handlerEnd atomic.Int64
	var trickles atomic.Int64
	var cancelledAt atomic.Int64
	var writeErr atomic.Value
	const trickleFor = 6 * time.Second
	h := stream.HandlerFunc(func(ctx context.Context, s *stream.Stream) error {
		st, ok := s.ResponseWriter.(stream.Streamer)
		if !ok {
			t.Errorf("no Streamer")
			return nil
		}
		if err := st.WriteHeader(s, 200, [][2]string{{"content-type", "application/octet-stream"}}); err != nil {
			writeErr.Store("hdr: " + err.Error())
			return nil
		}
		chunk := bytes.Repeat([]byte{'x'}, 1<<20)
		for i := 0; i < 96; i++ {
			if err := st.Write(s, chunk); err != nil {
				writeErr.Store("burst: " + err.Error())
				return nil
			}
		}
		burstDone.Store(time.Now().UnixNano())
		if trickle {
			small := bytes.Repeat([]byte{'y'}, 1024)
			for end := time.Now().Add(trickleFor); time.Now().Before(end); {
				if ctx.Err() != nil || s.IsCancelled() {
					cancelledAt.Store(time.Now().UnixNano())
					break
				}
				if err := st.Write(s, small); err != nil {
					writeErr.Store("trickle: " + err.Error())
					break
				}
				trickles.Add(1)
				time.Sleep(50 * time.Millisecond)
			}
		}
		handlerEnd.Store(time.Now().UnixNano())
		return nil
	})

	e, err := New(resource.Config{
		Addr:         addr,
		Protocol:     engine.H2C,
		ReadTimeout:  4 * time.Second,
		IdleTimeout:  4 * time.Second,
		WriteTimeout: time.Second,
		Resources:    resource.Resources{Workers: 2},
	}, asyncStreamHandler876{f: h})
	if err != nil {
		t.Fatalf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(30 * time.Second):
		}
	}()
	for dl := time.Now().Add(15 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<10)
		})
	}}
	c, err := d.Dial("tcp", e.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1<<31 - 1}); err != nil {
		t.Fatal(err)
	}
	if err := fr.WriteWindowUpdate(0, 1<<31-1-65535); err != nil {
		t.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: false, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	if err := fr.WriteData(1, true, nil); err != nil {
		t.Fatal(err)
	}
	sent := time.Now()
	// The client reads nothing from here on.
	for dl := time.Now().Add(5 * time.Second); e.Metrics().ActiveConnections == 0 && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	var closedAt, countedAt time.Time
	for dl := time.Now().Add(20 * time.Second); time.Now().Before(dl); {
		m := e.Metrics()
		if countedAt.IsZero() && m.CloseCount >= 1 {
			countedAt = time.Now()
		}
		if closedAt.IsZero() && m.ActiveConnections == 0 {
			closedAt = time.Now()
		}
		if !closedAt.IsZero() && !countedAt.IsZero() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !countedAt.IsZero() {
		t.Logf("CloseCount reached 1 at %v after the request; ActiveConnections reached 0 at %v", countedAt.Sub(sent).Round(time.Millisecond), closedAt.Sub(sent).Round(time.Millisecond))
	}
	if ca := cancelledAt.Load(); ca != 0 {
		t.Logf("handler saw its stream cancelled %v after the request", time.Unix(0, ca).Sub(sent).Round(time.Millisecond))
	}
	bd := burstDone.Load()
	var sinceBurst time.Duration
	if bd != 0 && !closedAt.IsZero() {
		sinceBurst = closedAt.Sub(time.Unix(0, bd))
	}
	t.Logf("trickle=%v: request at 0; burst done %s; conn closed %s after the request (%s after the burst); trickles=%d writeErr=%v BytesWritten=%d",
		trickle, func() string {
			if bd == 0 {
				return "never"
			}
			return time.Unix(0, bd).Sub(sent).Round(time.Millisecond).String()
		}(), closedAt.Sub(sent).Round(time.Millisecond), sinceBurst.Round(time.Millisecond), trickles.Load(), writeErr.Load(),
		e.Metrics().BytesWritten)
	if bd == 0 {
		t.Fatal("PREMISE: the burst never finished")
	}
	if closedAt.IsZero() {
		t.Fatalf("the conn was never closed within 20 s (peer reads nothing; WriteTimeout 1s, ReadTimeout 4s)")
	}
	if lim := 3 * time.Second; sinceBurst > lim {
		t.Errorf("the conn whose peer took nothing after the burst lived %v after it (> %v): it should be reaped about WriteTimeout (1s) after the refused write, and a clock restarted by each frame the handler queues (or ReadTimeout, 4s, as before celeris#876) holds it longer", sinceBurst.Round(time.Millisecond), lim)
	}
}

// asyncStreamHandler876 marks every route async, so the H2 handler runs on the worker
// pool, as an async route's (a streaming SSE handler's) does, not inline on the
// event loop.
type asyncStreamHandler876 struct{ f stream.HandlerFunc }

func (h asyncStreamHandler876) HandleStream(ctx context.Context, s *stream.Stream) error {
	return h.f(ctx, s)
}
func (asyncStreamHandler876) RouteAsync(string, string) bool { return true }
func (asyncStreamHandler876) HasAsyncRoutes() bool           { return true }
