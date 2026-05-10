//go:build validation

package validation

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Endpoint serves the snapshot over a unix-domain socket. There is at
// most one Endpoint per process; StartEndpoint enforces that with a
// package-level singleton. The socket path is the hard-coded
// SocketPath constant — not configurable, not exposed in production.
type Endpoint struct {
	listener net.Listener
	server   *http.Server
	done     chan struct{}
}

var (
	endpointMu       sync.Mutex
	endpointInstance *Endpoint
	endpointStarted  atomic.Bool
)

// StartEndpoint binds the unix socket at SocketPath and starts an
// HTTP server that returns Snapshot() as JSON on any GET request.
// Calling it more than once in the same process is a no-op (returns
// the running endpoint). The caller is expected to defer Stop on
// clean shutdown so the socket file is removed.
func StartEndpoint() (*Endpoint, error) {
	endpointMu.Lock()
	defer endpointMu.Unlock()

	if endpointInstance != nil {
		return endpointInstance, nil
	}

	// Best-effort cleanup of a stale socket from a previous crash:
	// unix.Listen fails with "address already in use" if the inode
	// exists, even when nothing is listening. Ignore the error from
	// os.Remove — a missing file is the happy path, and a permission
	// error will surface on the Listen call below with a clearer
	// message.
	_ = os.Remove(SocketPath)

	ln, err := net.Listen("unix", SocketPath)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Snapshot())
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	ep := &Endpoint{
		listener: ln,
		server:   srv,
		done:     make(chan struct{}),
	}

	go func() {
		defer close(ep.done)
		err := srv.Serve(ln)
		// http.ErrServerClosed is the expected shutdown signal; any
		// other error means the listener died unexpectedly and
		// callers polling the socket will see ECONNREFUSED.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Validation builds intentionally lack a logger
			// dependency to keep this package leaf-level; we swallow
			// the error rather than panic, on the principle that a
			// dead endpoint should not crash the system under test.
			_ = err
		}
	}()

	endpointInstance = ep
	endpointStarted.Store(true)
	return ep, nil
}

// Stop shuts the endpoint down, closes the listener, and removes the
// socket file. Safe to call from a defer; idempotent.
func (e *Endpoint) Stop(ctx context.Context) error {
	if e == nil {
		return nil
	}
	endpointMu.Lock()
	if endpointInstance == e {
		endpointInstance = nil
	}
	endpointMu.Unlock()

	err := e.server.Shutdown(ctx)
	_ = os.Remove(SocketPath)
	select {
	case <-e.done:
	case <-ctx.Done():
	}
	endpointStarted.Store(false)
	return err
}

// EndpointAddr returns the unix socket address the endpoint is bound
// to, or nil if no endpoint is running.
func EndpointAddr() net.Addr {
	endpointMu.Lock()
	defer endpointMu.Unlock()
	if endpointInstance == nil {
		return nil
	}
	return endpointInstance.listener.Addr()
}
