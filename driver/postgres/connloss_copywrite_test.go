package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// A COPY FROM whose connection goes away while its frames are written returns the
// connection-loss error (ErrClosed and io.ErrUnexpectedEOF, the write error still
// in the chain), not the bare write error. Which write fails (the CopyData frames,
// CopyDone, the first Query) depends on when the close lands, so the tests make it
// deterministic: the transport is closed by the caller's own source at a chosen
// row, so the very next write fails.

// closingSource is a CopyFromSource that closes the conn's transport when it
// delivers row closeAt (counting from 1), or on the call that reports the end of
// the rows when closeAt is -1 (so CopyDone is the first write that fails).
type closingSource struct {
	c       *pgConn
	n       int
	closeAt int
	i       int
}

func (s *closingSource) Next() bool {
	if s.i >= s.n {
		if s.closeAt == -1 {
			_ = s.c.tcp.Close()
		}
		return false
	}
	s.i++
	if s.i == s.closeAt {
		_ = s.c.tcp.Close()
	}
	return true
}
func (s *closingSource) Values() ([]any, error) { return []any{int64(s.i)}, nil }
func (s *closingSource) Err() error             { return nil }

func TestConnLossCopyFromWriteErrorIsLossError(t *testing.T) {
	for _, closeAt := range []int{-1, 1, 3, 50} {
		name := fmt.Sprintf("closeAtRow%d", closeAt)
		if closeAt == -1 {
			name = "closeBeforeCopyDone"
		}
		t.Run(name, func(t *testing.T) {
			addr := startFakePG(t, copyServer(t, copyComplete, 100, false))
			host, port, _ := net.SplitHostPort(addr)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := dialDirectConn(ctx, DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable"}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			src := &closingSource{c: c, n: 100, closeAt: closeAt}
			_, err = c.copyFrom(ctx, "t", []string{"g"}, src)
			if err == nil {
				t.Fatalf("copyFrom returned nil after its transport was closed (closeAt %d)", closeAt)
			}
			if !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error %v (%T) does not match ErrClosed and io.ErrUnexpectedEOF", err, err)
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("error %v matches io.EOF", err)
			}
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("error %v lost the write error (net.ErrClosed) from its chain", err)
			}
		})
	}
}

// The first write of COPY TO (the Query that starts it) and of COPY FROM, on a
// transport that is already closed: the same error.
func TestConnLossCopyStartWriteErrorIsLossError(t *testing.T) {
	for _, dir := range []string{"to", "from"} {
		t.Run(dir, func(t *testing.T) {
			addr := startFakePG(t, copyServer(t, copyComplete, 10, false))
			host, port, _ := net.SplitHostPort(addr)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := dialDirectConn(ctx, DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable"}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.tcp.Close()
			if dir == "to" {
				err = c.copyTo(ctx, "COPY (SELECT 1) TO STDOUT", func([]byte) error { return nil })
			} else {
				_, err = c.copyFrom(ctx, "t", []string{"g"}, CopyFromSlice([][]any{{int64(1)}}))
			}
			if err == nil {
				t.Fatal("nil error on a closed transport")
			}
			if !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error %v (%T) does not match ErrClosed and io.ErrUnexpectedEOF", err, err)
			}
		})
	}
}
