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
	"time"
)

// SocketPath is the hard-coded unix-socket path the validation
// endpoint listens on. Defined here under the validation build tag so
// production binaries do not embed the constant.
const SocketPath = "/tmp/celeris-validation.sock"

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
)

// StartEndpoint binds the unix socket at SocketPath and starts an
// HTTP server that returns Snapshot() as JSON on GET /snapshot (and
// the legacy GET / alias). Calling it more than once in the same
// process is a no-op (returns the running endpoint). The caller is
// expected to defer Stop on clean shutdown so the socket file is
// removed.
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
	// Restrict access to the owner — the snapshot is debug-only and
	// must not be readable by other users on a shared host.
	if cerr := os.Chmod(SocketPath, 0o600); cerr != nil {
		_ = ln.Close()
		_ = os.Remove(SocketPath)
		return nil, cerr
	}

	mux := http.NewServeMux()
	snapshotHandler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(Snapshot())
	}
	// Method-aware routing (Go 1.22+): GET /snapshot is the canonical
	// path; GET / is the legacy alias kept for the validator-checker
	// that compiled against the original endpoint.
	mux.HandleFunc("GET /snapshot", snapshotHandler)
	mux.HandleFunc("GET /{$}", snapshotHandler)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    4096,
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
	return ep, nil
}

// Stop shuts the endpoint down, closes the listener, and removes the
// socket file. Safe to call from a defer; idempotent. The mutex is
// held across Shutdown + Remove + done-wait so a concurrent
// StartEndpoint cannot observe a half-torn-down singleton.
func (e *Endpoint) Stop(ctx context.Context) error {
	if e == nil {
		return nil
	}
	endpointMu.Lock()
	defer endpointMu.Unlock()

	if endpointInstance == e {
		endpointInstance = nil
	}

	err := e.server.Shutdown(ctx)
	select {
	case <-e.done:
	case <-ctx.Done():
	}
	_ = os.Remove(SocketPath)
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
