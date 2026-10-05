package std

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
)

// fakeListener returns a scripted sequence of Accept results: a non-nil entry
// is returned as an Accept error, a nil entry yields a live (net.Pipe) conn.
type fakeListener struct {
	errs  []error
	calls atomic.Int32
}

func (f *fakeListener) Accept() (net.Conn, error) {
	i := int(f.calls.Add(1)) - 1
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	c, _ := net.Pipe()
	return c, nil
}

func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A transient (non-ErrClosed) Accept error must be retried, not surfaced — net/http
// would otherwise treat it as fatal and return from Serve, killing the server.
func TestResilientListener_RetriesTransientAcceptErrors(t *testing.T) {
	transient := errors.New("accept tcp: connection reset by peer")
	f := &fakeListener{errs: []error{transient, transient, nil}}
	rl := &resilientListener{Listener: f, logger: discardLogger()}

	conn, err := rl.Accept()
	if err != nil {
		t.Fatalf("expected a conn after transient retries, got error: %v", err)
	}
	if conn == nil {
		t.Fatal("expected a non-nil conn")
	}
	_ = conn.Close()
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("expected 3 Accept calls (2 retries + 1 success), got %d", got)
	}
}

// A closed listener (graceful Shutdown) must propagate so net/http's Serve
// returns ErrServerClosed and stops cleanly instead of looping forever.
func TestResilientListener_PropagatesClosed(t *testing.T) {
	f := &fakeListener{errs: []error{net.ErrClosed}}
	rl := &resilientListener{Listener: f, logger: discardLogger()}

	if _, err := rl.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed to propagate, got %v", err)
	}
}
