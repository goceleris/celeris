//go:build linux

package epoll

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
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
// h2Conns pass re-entered closeWhenFlushed for every one (it no longer does,
// celeris#885 item 38, but markClosing still guards the same clock against the
// EPOLLRDHUP re-entry), and each re-entry must not restart the drain clock (a
// restart without progress made the conn immortal for as long as the handler
// wrote). Before the fix of that restamp
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

// celeris#885 item 38, the epoll side of what io_uring does at once. An h2c
// conn whose write was refused (writeRefused: a frame the loop could not queue
// was lost) is closing, but it stayed on h2Conns and went on taking the frames
// its handler queues: once the peer read the backlog down below the cap, the
// next frame was appended behind it, and the h2Conns pass re-entered
// closeWhenFlushed (an EPOLL_CTL_MOD) for every frame. io_uring's makeWriteFn
// returns at once on a closing conn. The conn must send nothing the handler
// writes after the refusal.
//
// The peer here READS: it sits out the burst, which the loop refuses past the
// 64 MiB cap, then reads the backlog at about 64 MiB/s while the handler (an
// SSE-like stream) goes on writing 1 KiB of 'y' every 50 ms. A conn that keeps
// taking those frames delivers some of them behind the backlog; one that has
// dropped them delivers none. (The silent peer of the test above cannot tell
// the two: it takes nothing, and the backlog never drops under the cap.)
func TestEngineH2ClosingConnTakesNoFrameTheHandlerWritesAfterTheRefusal885(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var burstDone atomic.Int64
	var trickles atomic.Int64
	var writeErr atomic.Value
	const trickleFor = 8 * time.Second
	// The close is asked once, however many frames the handler writes after:
	// each further request is an EPOLL_CTL_MOD (celeris#885 item 38).
	// Counted for the first conn the hook sees: loops of an earlier test's
	// engine may still be winding down, and a count over the process would
	// include their closes.
	var closeAsks atomic.Int64
	var asked atomic.Pointer[connState]
	askHook := func(cs *connState) {
		if cs.h2State == nil {
			return // this test's one conn is an h2c conn
		}
		asked.CompareAndSwap(nil, cs)
		if asked.Load() == cs {
			closeAsks.Add(1)
		}
	}
	testHookCloseWhenFlushed.Store(&askHook)
	t.Cleanup(func() { testHookCloseWhenFlushed.Store(nil) }) // runs after the engine is stopped
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
		small := bytes.Repeat([]byte{'y'}, 1024)
		for end := time.Now().Add(trickleFor); time.Now().Before(end); {
			if ctx.Err() != nil || s.IsCancelled() {
				break
			}
			if err := st.Write(s, small); err != nil {
				writeErr.Store("trickle: " + err.Error())
				break
			}
			trickles.Add(1)
			time.Sleep(50 * time.Millisecond)
		}
		return nil
	})

	e, err := New(resource.Config{
		Addr:         addr,
		Protocol:     engine.H2C,
		ReadTimeout:  30 * time.Second,
		IdleTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Second,
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
	c, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fw := http2.NewFramer(c, nil)
	if err := fw.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1<<31 - 1}); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteWindowUpdate(0, 1<<31-1-65535); err != nil {
		t.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fw.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: false, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteData(1, true, nil); err != nil {
		t.Fatal(err)
	}

	// The peer reads nothing until the burst is over and the handler has been
	// refused for a while (the backlog is over the cap, the trickle frames
	// that came meanwhile were lost with it).
	for dl := time.Now().Add(20 * time.Second); burstDone.Load() == 0 && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	if burstDone.Load() == 0 {
		t.Fatal("PREMISE: the burst never finished")
	}
	time.Sleep(500 * time.Millisecond)
	tricklesAtRead := trickles.Load()

	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	fr := http2.NewFramer(nil, bufio.NewReaderSize(c, 1<<20))
	fr.SetMaxReadFrameSize(1 << 24)
	var data, yBytes, sinceNap int
	readStart := time.Now()
	var endErr error
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			endErr = err
			break
		}
		df, ok := f.(*http2.DataFrame)
		if !ok {
			continue
		}
		d := df.Data()
		data += len(d)
		yBytes += bytes.Count(d, []byte{'y'})
		if sinceNap += len(d); sinceNap >= 1<<20 { // about 64 MiB/s
			sinceNap = 0
			time.Sleep(15 * time.Millisecond)
		}
	}
	readTook := time.Since(readStart)
	trickledWhileReading := trickles.Load() - tricklesAtRead
	t.Logf("peer read %d DATA bytes of %d sent by the burst in %v (end: %v); 'y' bytes among them: %d; handler wrote %d trickles meanwhile; writeErr=%v",
		data, 96<<20, readTook.Round(time.Millisecond), endErr, yBytes, trickledWhileReading, writeErr.Load())

	if data >= 96<<20 || data < 32<<20 {
		t.Fatalf("PREMISE: the peer received %d bytes of the %d burst: the write must have been refused (less than all) with a backlog to read (more than 32 MiB)", data, 96<<20)
	}
	if trickledWhileReading < 5 {
		t.Fatalf("PREMISE: the handler wrote %d trickle frames while the peer read for %v; the frames to be taken or dropped never came", trickledWhileReading, readTook.Round(time.Millisecond))
	}
	// The conn is cut once the backlog is read (about a second here), not at
	// the 20 s read deadline: a conn kept open for as long as the handler
	// streams (the trickle runs 8 s) is the harm the item names.
	if !errors.Is(endErr, io.EOF) {
		t.Errorf("the peer's read ended with %v after %v, want io.EOF: the conn must be closed once the backlog is delivered", endErr, readTook.Round(time.Millisecond))
	}
	if lim := 5 * time.Second; readTook > lim {
		t.Errorf("the closing conn stayed open %v while the peer read (> %v): it must close as soon as the backlog is delivered, not live as long as the handler streams", readTook.Round(time.Millisecond), lim)
	}
	if n := closeAsks.Load(); n != 1 {
		t.Errorf("closeWhenFlushed ran %d times for one conn closing on a refused write (%d trickle frames queued after it): once, not per frame", n, trickles.Load())
	}
	if yBytes != 0 {
		t.Errorf("the closing conn delivered %d bytes of what the handler wrote after the refusal (%d trickle frames while the peer read): "+
			"a conn closing on a refused write must drop its handler's frames, as io_uring does", yBytes, trickledWhileReading)
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
