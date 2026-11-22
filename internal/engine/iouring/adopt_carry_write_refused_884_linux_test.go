//go:build linux

package iouring

import (
	"bufio"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#884 (item 38 of its list): the replay of a transplant's
// carried request must end a conn whose write was refused, as handleRecv's
// tail does (respondAndArm). An HTTP/1 StreamWriter whose staged response
// passes maxSendQueueBytesDetached is refused by StreamWriteErr, which sets
// writeRefused, while nothing has been sent (the response is staged whole
// until the handler returns). Neither carried-replay tail read the flag: the
// conn was left open with its response cut off, until the client's next
// request or the conn's timeout.

// refused884Handler answers /stream and /stream-close by writing 1 MiB chunks
// until the engine refuses one, and every other path as carry543Handler does.
// /stream returns at the refusal: ProcessH1 then cuts the response and ends the
// conn itself (the open stream's Close is refused), so the replay's verdict
// closes it already. /stream-close closes its writer first, as a handler that
// defers the Close does: the stream is then no longer open, ProcessH1 returns
// no verdict, and only writeRefused says the response is cut.
type refused884Handler struct {
	*carry543Handler
	refused chan error
}

func (h *refused884Handler) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path != "/stream" && s.Path != "/stream-close" {
		return h.carry543Handler.HandleStream(ctx, s)
	}
	st, ok := s.ResponseWriter.(stream.Streamer)
	if !ok {
		return nil
	}
	if err := st.WriteHeader(s, 200, [][2]string{{"content-type", "application/octet-stream"}}); err != nil {
		return nil
	}
	chunk := make([]byte, 1<<20)
	for range 2 * (maxSendQueueBytesDetached >> 20) { // never loops that far: the 65th chunk is refused
		if err := st.Write(s, chunk); err != nil {
			select {
			case h.refused <- err:
			default:
			}
			if s.Path == "/stream-close" {
				_ = st.Close(s)
			}
			return nil
		}
	}
	return nil
}

func start884Refused(t *testing.T, async bool) (*Engine, *refused884Handler) {
	t.Helper()
	h := &refused884Handler{carry543Handler: newCarry543Handler(), refused: make(chan error, 1)}
	addr := freeAddr639(t)
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		return New(resource.Config{
			Addr: addr, Protocol: engine.HTTP1, AsyncHandlers: async,
			Resources: resource.Resources{Workers: 2},
		}, h)
	})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return e, h
}

// testCarriedStreamRefusedCloses884 adopts a conn whose carried request is
// /stream and reads to the end of the conn: what was staged before the
// refusal is delivered, then the conn is closed. On main the client got the
// same bytes and then nothing: the conn stayed open.
func testCarriedStreamRefusedCloses884(t *testing.T, e *Engine, h *refused884Handler, path string) {
	t.Helper()
	client, fd := adoptable543(t)
	if err := e.AdoptConn(fd, engine.Carryover{
		RemoteAddr: client.LocalAddr().String(),
		Buffered:   []byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n"),
	}); err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("AdoptConn with a carry: %v", err)
	}
	select {
	case err := <-h.refused:
		t.Logf("the handler's write was refused: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("apparatus: the handler was never refused a write (the carried request did not run, or did not stage the response)")
	}
	br := bufio.NewReaderSize(client, 1<<20)
	_ = client.SetReadDeadline(time.Now().Add(20 * time.Second))
	n, err := io.Copy(io.Discard, br)
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("celeris#884: the conn whose carried request's response was refused stayed open: read %d bytes, then %v "+
			"(want EOF: the replay's tail did not close it, as handleRecv's does)", n, err)
	}
	if n < maxSendQueueBytesDetached-(2<<20) {
		t.Errorf("the client got %d bytes of the staged response, want about %d: what was staged must go out before the close",
			n, maxSendQueueBytesDetached)
	}
}

func TestIouringAdoptCarriedStreamRefusedClosesSync884(t *testing.T) {
	for _, path := range []string{"/stream", "/stream-close"} {
		t.Run(path, func(t *testing.T) {
			e, h := start884Refused(t, false)
			testCarriedStreamRefusedCloses884(t, e, h, path)
		})
	}
}

func TestIouringAdoptCarriedStreamRefusedClosesAsync884(t *testing.T) {
	for _, path := range []string{"/stream", "/stream-close"} {
		t.Run(path, func(t *testing.T) {
			e, h := start884Refused(t, true)
			testCarriedStreamRefusedCloses884(t, e, h, path)
		})
	}
}
