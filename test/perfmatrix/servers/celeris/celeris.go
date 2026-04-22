// Package celeris registers every benched celeris configuration
// (Engine × Protocol × Upgrade × Async → 35 distinct cell-columns) as a
// [servers.Server] against the perfmatrix registry.
//
// Naming convention (stable, script-friendly, all lowercase):
//
//	celeris-<engine>-<protocol>[+upg|-noupg][-sync|-async]
//
// Examples:
//
//	celeris-std-h1
//	celeris-std-h2c+upg
//	celeris-iouring-auto-noupg-async
//	celeris-adaptive-h2c+upg-sync
//
// HTTP1 has no upgrade suffix (upgrade is inherently H1→H2). Std has no
// sync/async suffix (it always uses Go's native goroutine-per-conn model
// and does not expose the AsyncHandlers toggle).
package celeris

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/driver/postgres"
	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// Engines enumerates the celeris engines sampled by the matrix. Order
// is stable so cell-column IDs are reproducible.
var Engines = []string{"std", "epoll", "iouring", "adaptive"}

// Protocols enumerates the protocols sampled by the matrix.
var Protocols = []string{"h1", "h2c", "auto"}

// payloadSmall is the small JSON body returned by /json.
type payloadSmall struct {
	Message string `json:"message"`
	Server  string `json:"server"`
}

// payload1K is a ~1 KiB JSON document built once at init().
var payload1K = buildJSONPayload(1024)

// payload64K is a ~64 KiB JSON document built once at init().
var payload64K = buildJSONPayload(64 * 1024)

// buildJSONPayload returns a byte slice that, when marshalled as JSON
// inside an object wrapper, totals approximately targetBytes. The shape
// is {"size":N,"data":"aaa...aaa"} so downstream consumers can parse it
// the same way regardless of size.
func buildJSONPayload(targetBytes int) []byte {
	// Account for the JSON envelope (~32 bytes of keys/quotes/commas/brackets).
	const envelope = 32
	dataLen := targetBytes - envelope
	if dataLen < 1 {
		dataLen = 1
	}
	data := make([]byte, dataLen)
	for i := range data {
		data[i] = 'a'
	}
	out, err := json.Marshal(struct {
		Size int    `json:"size"`
		Data string `json:"data"`
	}{
		Size: dataLen,
		Data: string(data),
	})
	if err != nil {
		// json.Marshal on a simple struct of ints+strings cannot fail in
		// practice; panic here makes the bug loud rather than returning a
		// silently-wrong payload.
		panic(fmt.Sprintf("celeris perfmatrix: build payload failed: %v", err))
	}
	return out
}

// ptrBool returns a pointer to b. Used to populate celeris.Config.EnableH2Upgrade,
// which is a tri-state *bool (nil / &true / &false).
func ptrBool(b bool) *bool { return &b }

// celerisServer is a single cell-column of the matrix. Each call to
// newCelerisServer produces one distinct [servers.Server] that, once
// started, binds a fresh ephemeral port.
type celerisServer struct {
	name     string
	engine   celeris.EngineType
	protocol celeris.Protocol

	// h2cUpgrade is nil when the knob is not meaningful (HTTP1 cells)
	// or a non-nil pointer for H2C / Auto cells that want to force the
	// upgrade flag on or off.
	h2cUpgrade *bool

	asyncHandlers bool

	features servers.FeatureSet

	mu  sync.Mutex
	srv *celeris.Server
	ln  net.Listener
	// listenDone fires after srv.StartWithListener returns, whether by
	// Shutdown (nil err) or by a fatal bind/accept failure.
	listenDone chan error

	// Driver clients instantiated by mountDriverHandlers. Nil when
	// svcs is nil or the relevant driver was not provisioned. Closed
	// in Stop via shutdownDriverHandlers so repeat lifecycles don't
	// leak pool connections.
	pgPool      *postgres.Pool
	redisClient *redis.Client
	mcClient    *memcached.Client
	sessionMW   celeris.HandlerFunc
}

// Name implements [servers.Server].
func (s *celerisServer) Name() string { return s.name }

// Kind implements [servers.Server].
func (s *celerisServer) Kind() string { return "celeris" }

// Features implements [servers.Server].
func (s *celerisServer) Features() servers.FeatureSet { return s.features }

// Start implements [servers.Server]. It wires the six static handlers,
// pre-binds an ephemeral-port TCP listener, hands it to celeris via
// StartWithListener (which covers both the std path and the native
// engines' SO_REUSEPORT rebind), then blocks until the server's own
// Addr() is non-nil so the caller can safely dial the returned listener
// without a race.
func (s *celerisServer) Start(_ context.Context, svcs *services.Handles) (net.Listener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srv != nil {
		return nil, fmt.Errorf("celeris perfmatrix: %s already started", s.name)
	}

	// Bind the listener FIRST so we can hand celeris an Addr that matches
	// it exactly. celeris.Config.WithDefaults unconditionally fills in
	// ":8080" when Addr is empty; the Adaptive engine's sub-engines
	// (epoll + iouring) then reject the config as "ambiguous" because
	// the listener they're handed isn't bound to ":8080". Setting Addr to
	// the listener's actual address keeps the validator happy across all
	// four engines (std silently ignores Addr when Listener is set;
	// native engines tolerate exact match).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("celeris perfmatrix: %s bind: %w", s.name, err)
	}

	cfg := celeris.Config{
		Addr:            ln.Addr().String(),
		Engine:          s.engine,
		Protocol:        s.protocol,
		EnableH2Upgrade: s.h2cUpgrade,
		AsyncHandlers:   s.asyncHandlers,
	}
	srv := celeris.New(cfg)
	registerStaticHandlers(srv)
	// Wave-3 additions: driver-backed and middleware-chain scenarios.
	// Drivers are lazily constructed from svcs; when svcs is nil or a
	// service is absent, the driver handler returns 503 so the
	// orchestrator can detect missing prerequisites. Chain handlers are
	// always registered — they need no external services.
	mountDriverHandlers(s, srv, svcs)
	mountChainHandlers(srv)

	done := make(chan error, 1)
	go func() { done <- srv.StartWithListener(ln) }()

	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-done:
			_ = ln.Close()
			return nil, fmt.Errorf("celeris perfmatrix: %s exited early: %w", s.name, err)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		_ = ln.Close()
		_ = srv.Shutdown(context.Background())
		return nil, fmt.Errorf("celeris perfmatrix: %s did not bind within deadline", s.name)
	}

	s.srv = srv
	// On the native engines StartWithListener closes ln after extracting
	// the port; on std it keeps ln. Either way the cell-column's target
	// address is srv.Addr(), so we return a dialable wrapper rather than
	// the raw ln (which may be closed).
	s.ln = &addrListener{addr: srv.Addr(), closed: make(chan struct{})}
	s.listenDone = done
	return s.ln, nil
}

// Stop implements [servers.Server].
func (s *celerisServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	ln := s.ln
	done := s.listenDone
	s.srv = nil
	s.ln = nil
	s.listenDone = nil
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if ln != nil {
		_ = ln.Close()
	}
	if done != nil {
		// Drain — Listen returns nil on graceful Shutdown.
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	// Close any driver clients opened by mountDriverHandlers so repeat
	// Start/Stop cycles don't leak pool connections.
	shutdownDriverHandlers(s)
	return err
}

// addrListener is a no-accept, no-dial [net.Listener] that only reports
// an address. The perfmatrix orchestrator uses Listener.Addr() to learn
// where to dial; it never calls Accept. Close is idempotent and
// terminates any pending Accept (none, in practice).
type addrListener struct {
	addr   net.Addr
	closed chan struct{}
	once   sync.Once
}

// Accept blocks until Close is called, then returns an error. The
// perfmatrix orchestrator never calls Accept, but satisfying the
// interface correctly makes this safe if that assumption changes.
func (a *addrListener) Accept() (net.Conn, error) {
	<-a.closed
	return nil, net.ErrClosed
}

// Close releases any pending Accept call.
func (a *addrListener) Close() error {
	a.once.Do(func() { close(a.closed) })
	return nil
}

// Addr returns the celeris server's bound address.
func (a *addrListener) Addr() net.Addr { return a.addr }

// registerStaticHandlers wires the six static scenario endpoints. The
// driver and middleware agents (wave 2C/2D) layer their own endpoints
// on top of the returned *celeris.Server via the Features.Drivers /
// Features.Middleware flags.
func registerStaticHandlers(srv *celeris.Server) {
	srv.GET("/", func(c *celeris.Context) error {
		return c.StatusBlob("text/plain; charset=utf-8", []byte("Hello, World!"))
	})

	srv.GET("/json", func(c *celeris.Context) error {
		return c.JSON(200, payloadSmall{
			Message: "Hello, World!",
			Server:  "celeris",
		})
	})

	srv.GET("/json-1k", func(c *celeris.Context) error {
		return c.StatusBlob("application/json", payload1K)
	})

	srv.GET("/json-64k", func(c *celeris.Context) error {
		return c.StatusBlob("application/json", payload64K)
	})

	srv.GET("/users/:id", func(c *celeris.Context) error {
		return c.StatusBlob("text/plain; charset=utf-8",
			[]byte("User ID: "+c.Param("id")))
	})

	srv.POST("/upload", func(c *celeris.Context) error {
		// Touch the body so keep-alive accounting sees the request as fully
		// consumed. Celeris parsers deliver the body eagerly, so a call to
		// Body() is sufficient on every engine + protocol combination.
		_ = c.Body()
		return c.StatusBlob("text/plain; charset=utf-8", []byte("OK"))
	})
}

// protocolFor maps the string protocol keyword to the celeris Protocol
// constant. Panics on unknown input — the init() only feeds it literals
// from Protocols.
func protocolFor(p string) celeris.Protocol {
	switch p {
	case "h1":
		return celeris.HTTP1
	case "h2c":
		return celeris.H2C
	case "auto":
		return celeris.Auto
	}
	panic("celeris perfmatrix: unknown protocol " + p)
}

// engineFor maps the string engine keyword to the celeris EngineType
// constant. Panics on unknown input.
func engineFor(e string) celeris.EngineType {
	switch e {
	case "std":
		return celeris.Std
	case "epoll":
		return celeris.Epoll
	case "iouring":
		return celeris.IOUring
	case "adaptive":
		return celeris.Adaptive
	}
	panic("celeris perfmatrix: unknown engine " + e)
}

// newCelerisServer assembles one configured cell-column.
//
//   - engine / protocol: keywords from Engines / Protocols.
//   - upgrade: "" when inapplicable (HTTP1), "+upg" to force on, "-noupg" to force off.
//   - async: only "std" honors "" (no async toggle); every other engine
//     uses "-sync" or "-async".
func newCelerisServer(engine, protocol, upgrade, async string) *celerisServer {
	name := "celeris-" + engine + "-" + protocol
	if upgrade != "" {
		name += upgrade
	}
	if async != "" {
		name += async
	}

	var upgradePtr *bool
	switch upgrade {
	case "+upg":
		upgradePtr = ptrBool(true)
	case "-noupg":
		upgradePtr = ptrBool(false)
	case "":
		upgradePtr = nil
	default:
		panic("celeris perfmatrix: unknown upgrade token " + upgrade)
	}

	isAsync := async == "-async"

	feat := servers.FeatureSet{
		HTTP1:         true,
		Drivers:       true,
		Middleware:    true,
		AsyncHandlers: isAsync,
	}
	switch protocol {
	case "h2c":
		feat.HTTP2C = true
	case "auto":
		feat.Auto = true
		feat.HTTP2C = true
	}
	if upgradePtr != nil && *upgradePtr {
		feat.H2CUpgrade = true
	}

	return &celerisServer{
		name:          name,
		engine:        engineFor(engine),
		protocol:      protocolFor(protocol),
		h2cUpgrade:    upgradePtr,
		asyncHandlers: isAsync,
		features:      feat,
	}
}

// register registers every (engine, protocol, upgrade, async) combination
// that the matrix exercises. See the 35-row table in the package doc
// comment for the authoritative config list.
func register() {
	// std: no async toggle (always goroutine-per-conn natively).
	servers.Register(newCelerisServer("std", "h1", "", ""))
	servers.Register(newCelerisServer("std", "h2c", "+upg", ""))
	servers.Register(newCelerisServer("std", "h2c", "-noupg", ""))
	servers.Register(newCelerisServer("std", "auto", "+upg", ""))
	servers.Register(newCelerisServer("std", "auto", "-noupg", ""))

	// epoll / iouring / adaptive — Linux-only at registration time.
	if runtime.GOOS != "linux" {
		return
	}

	for _, eng := range []string{"epoll", "iouring", "adaptive"} {
		for _, async := range []string{"-sync", "-async"} {
			servers.Register(newCelerisServer(eng, "h1", "", async))
			servers.Register(newCelerisServer(eng, "h2c", "+upg", async))
			servers.Register(newCelerisServer(eng, "h2c", "-noupg", async))
			servers.Register(newCelerisServer(eng, "auto", "+upg", async))
			servers.Register(newCelerisServer(eng, "auto", "-noupg", async))
		}
	}
}

func init() { register() }
