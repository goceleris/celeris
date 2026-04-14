package websocket

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestCloseDuringWrite races Conn.Close() against in-flight WriteMessage
// calls. The expected outcome is no panic, no goroutine leak, and a
// timely return from both sides. Run with `-race` to catch data races
// between the close path and the write semaphore / bufio.Writer flushes.
func TestCloseDuringWrite(t *testing.T) {
	t.Parallel()

	clientPipe, serverPipe := net.Pipe()
	defer func() {
		_ = clientPipe.Close()
		_ = serverPipe.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newConn(ctx, cancel, serverPipe, 1024, 1024)
	srv.subprotocol = "test"

	var wg sync.WaitGroup
	wg.Add(3)

	// Drain client side so writes can complete; net.Pipe is unbuffered.
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			_ = clientPipe.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			_, err := clientPipe.Read(buf)
			if err != nil {
				if err == io.EOF || isTimeout(err) {
					if err == io.EOF {
						return
					}
					continue
				}
				return
			}
		}
	}()

	// Writer: hammers WriteMessage until error.
	go func() {
		defer wg.Done()
		payload := []byte("hammer")
		for {
			if err := srv.WriteMessage(OpText, payload); err != nil {
				return
			}
		}
	}()

	// Closer: close after a short delay.
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		_ = srv.Close()
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("close-during-write deadlock — goroutines did not unblock within 2s")
	}
}

func isTimeout(err error) bool {
	type timeouter interface{ Timeout() bool }
	t, ok := err.(timeouter)
	return ok && t.Timeout()
}
