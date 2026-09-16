package resource

import (
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"time"

	"github.com/goceleris/celeris/engine"
)

// defaultEngine returns Adaptive on Linux and Std on other platforms.
func defaultEngine() engine.EngineType {
	if runtime.GOOS == "linux" {
		return engine.Adaptive
	}
	return engine.Std
}

// Config holds the internal server configuration used by engine implementations.
// Users typically interact with the top-level celeris.Config instead.
type Config struct {
	// Protocol is the HTTP protocol version (HTTP1, H2C, or Auto).
	Protocol engine.Protocol
	// Engine is the I/O engine type (IOUring, Epoll, Adaptive, or Std).
	Engine engine.EngineType
	// Addr is the TCP address to listen on (e.g. ":8080").
	Addr string
	// Resources holds worker, buffer, and connection limit overrides.
	Resources Resources
	// MaxHeaderBytes is the max header block size in bytes (min 4096 if set).
	MaxHeaderBytes int
	// MaxConcurrentStreams limits simultaneous H2 streams per connection.
	MaxConcurrentStreams uint32
	// MaxFrameSize is the max H2 frame payload size (range 16384-16777215).
	MaxFrameSize uint32
	// InitialWindowSize is the H2 initial stream flow-control window size.
	InitialWindowSize uint32
	// ReadTimeout is the max duration for reading the entire request.
	// 0 asks for the default (60s); -1 disables it. After WithDefaults
	// the field is either > 0 or 0, which every consumer reads as
	// "disabled".
	ReadTimeout time.Duration
	// ReadHeaderTimeout is the max duration for reading the request
	// headers ONLY (status line + headers + final CRLF). A short value
	// here is the canonical defence against slowloris-style DoS:
	// clients that dribble headers byte-by-byte get their connection
	// killed within ReadHeaderTimeout instead of holding a goroutine
	// and a listener-backlog slot for the full ReadTimeout. The std
	// engine wires this to http.Server.ReadHeaderTimeout. The iouring
	// and epoll engines enforce it inside their H1 header read loop.
	//
	// Note: iouring/epoll's own SO_REUSEPORT-fronted multi-worker
	// design absorbs a lot of slowloris pressure through queue
	// scaling (16 workers × 4096 backlog ≈ 64 k slots). Std is on
	// a single net.Listen() queue and falls over much sooner. The
	// fix matters most for std but is sound on all engines.
	//
	// 0 asks for the default (10s); -1 disables it (0 after WithDefaults).
	ReadHeaderTimeout time.Duration
	// WriteTimeout is the max duration for writing the response.
	// 0 asks for the default (60s); -1 disables it (0 after WithDefaults).
	WriteTimeout time.Duration
	// IdleTimeout is the max duration a keep-alive connection may be idle.
	// 0 asks for the default (600s); -1 disables it (0 after WithDefaults).
	IdleTimeout time.Duration
	// DisableKeepAlive disables HTTP keep-alive.
	DisableKeepAlive bool
	// DisableDeferAccept turns TCP_DEFER_ACCEPT off on the listen sockets the
	// epoll and io_uring engines create. Default false: the option stays on.
	//
	// While it is set the kernel keeps a connection whose handshake has
	// completed but which has sent no data OUT of the accept queue entirely --
	// it stays a TCP_NEW_SYN_RECV request socket and accept4 answers EAGAIN.
	// That saves the engine a wakeup per idle connection, and it also hides
	// such a connection from PauseAccept's acceptQueuedOnPause drain: when the
	// pause closes the listen socket the request socket is orphaned and the
	// client's first request is met with a reset (celeris#662).
	//
	// Set this whenever the engine will pause accept. adaptive.New sets it on
	// both sub-engines WHEN that engine can actually switch, because every
	// switch pauses the outgoing one -- and leaves the option alone when no
	// switch is reachable (an old kernel, RLIMIT_MEMLOCK below one io_uring
	// worker's rings, or Protocol H2C), since such an engine never pauses and
	// so can never hit celeris#662. A standalone engine whose owner calls
	// PauseAccept (or celeris.Server.PauseAccept) should set it too; that it
	// is not the default there is tracked as celeris#675.
	//
	// It is off by default because the cost was measured before the default
	// was chosen -- 30 rounds per arm, two memlock shapes, A/A floors under
	// 1.3%: clearing the option costs +14-21% ns/op and +2.4-3.5 us of server
	// CPU per connection on churn where the request follows the handshake
	// promptly, and +67-200% on a connection that never sends. Keep-alive
	// traffic, where one accept is amortised over many requests, is unaffected
	// in both units.
	//
	// The option is also a free connect-and-never-send shield, which is a
	// RESOURCE question and not only a throughput one: while it is set such a
	// connection never becomes a socket the engine owns, so it costs no
	// descriptor, no conn-table slot and no buffer. Clearing it turns each
	// one into a real accepted connection -- on the ChurnSilent benchmark at
	// the 128 MiB memlock shape, epoll goes from 25 to 36 allocs/op and from
	// 1485 to 3644 B/op (+44% and +145%, n=30 rounds per arm). A deployment
	// that clears the option and faces untrusted clients is relying on its
	// conn-table cap and ReadHeaderTimeout for that protection instead.
	DisableDeferAccept bool
	// Listener is an optional pre-existing listener for socket inheritance.
	Listener net.Listener
	// MaxRequestBodySize is the maximum allowed request body size in bytes.
	// 0 uses the default (100 MB). -1 disables the limit (unlimited).
	MaxRequestBodySize int64
	// AsyncHandlers dispatches HTTP handlers to spawned goroutines instead
	// of inline execution on LockOSThread'd workers. See celeris.Config.
	AsyncHandlers bool
	// OnExpectContinue is called when an H1 request contains "Expect: 100-continue".
	// If the callback returns false, the server responds with 417 Expectation Failed
	// and skips reading the request body. If nil, the server always sends 100 Continue.
	OnExpectContinue func(method, path string, headers [][2]string) bool
	// OnConnect is called when a new connection is accepted.
	OnConnect func(addr string)
	// OnDisconnect is called when a connection is closed.
	OnDisconnect func(addr string)
	// Logger is the structured logger for engine diagnostics (default slog.Default()).
	Logger *slog.Logger
	// EnableH2Upgrade enables RFC 7540 §3.2 HTTP/1.1→H2C upgrades. Resolved
	// from celeris.Config.EnableH2Upgrade (pointer, may be nil) and Protocol.
	// Always a concrete value after WithDefaults.
	EnableH2Upgrade bool

	// defaulted records that WithDefaults has already resolved this
	// Config's sentinel fields, which is what makes a second pass a
	// no-op (celeris#594).
	//
	// The timeouts and MaxRequestBodySize accept a negative sentinel
	// meaning "disabled", and WithDefaults collapses it to 0 because 0
	// is what every consumer already reads as "off" (`> 0` guards in
	// the iouring/epoll loops, http.Server's own `d > 0` checks in std).
	// But 0 on the way *in* means "give me the default", so the mapping
	// is not idempotent on its own — and normalisation runs at least
	// twice on every start: Server.doPrepare normalises, then each
	// engine's New normalises again (three times through adaptive,
	// which normalises before building its sub-engine). That turned a
	// documented ReadHeaderTimeout=-1 into the 10s default.
	//
	// The marker distinguishes "0 = the caller wants the default" from
	// "0 = already resolved to disabled". It is unexported, so a Config
	// a caller builds as a literal always starts un-normalised and gets
	// the full default pass; it travels with the value on copy.
	defaulted bool
}

// Validate checks all config fields and returns any validation errors.
func (c Config) Validate() []error {
	var errs []error

	if c.Addr != "" {
		_, port, err := net.SplitHostPort(c.Addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid addr %q: %w", c.Addr, err))
		} else {
			var p int
			if _, err := fmt.Sscanf(port, "%d", &p); err != nil || p < 0 || p > 65535 {
				errs = append(errs, fmt.Errorf("port must be 0-65535, got %q", port))
			}
		}
	}

	if c.MaxFrameSize != 0 && (c.MaxFrameSize < 16384 || c.MaxFrameSize > 16777215) {
		errs = append(errs, fmt.Errorf("maxFrameSize must be 16384-16777215, got %d", c.MaxFrameSize))
	}

	if c.InitialWindowSize > 2147483647 {
		errs = append(errs, fmt.Errorf("initialWindowSize must be 0-2147483647, got %d", c.InitialWindowSize))
	}

	if c.MaxConcurrentStreams > 0x7fffffff {
		errs = append(errs, fmt.Errorf("maxConcurrentStreams must be <= 2147483647, got %d", c.MaxConcurrentStreams))
	}

	if c.MaxHeaderBytes != 0 && c.MaxHeaderBytes < 4096 {
		errs = append(errs, fmt.Errorf("maxHeaderBytes must be >= 4096 if set, got %d", c.MaxHeaderBytes))
	}

	if c.Resources.Workers != 0 && c.Resources.Workers < MinWorkers {
		errs = append(errs, fmt.Errorf("workers must be >= %d if set, got %d", MinWorkers, c.Resources.Workers))
	}

	if c.Resources.BufferSize != 0 && c.Resources.BufferSize < MinBufferSize {
		errs = append(errs, fmt.Errorf("bufferSize must be >= %d if set, got %d", MinBufferSize, c.Resources.BufferSize))
	}

	if c.Resources.MemoryLimitBytes < 0 {
		errs = append(errs, fmt.Errorf("memoryLimitBytes must be >= 0 (0 = unset), got %d", c.Resources.MemoryLimitBytes))
	}

	if c.ReadTimeout < -1 {
		errs = append(errs, fmt.Errorf("readTimeout must be >= -1, got %v", c.ReadTimeout))
	}
	if c.WriteTimeout < -1 {
		errs = append(errs, fmt.Errorf("writeTimeout must be >= -1, got %v", c.WriteTimeout))
	}
	if c.IdleTimeout < -1 {
		errs = append(errs, fmt.Errorf("idleTimeout must be >= -1, got %v", c.IdleTimeout))
	}

	if runtime.GOOS != "linux" {
		if c.Engine == engine.IOUring || c.Engine == engine.Epoll {
			errs = append(errs, fmt.Errorf("engine %s requires Linux", c.Engine))
		}
		if c.Engine == engine.Adaptive {
			errs = append(errs, fmt.Errorf("engine adaptive requires Linux"))
		}
	}

	// Listener + explicit Addr with a concrete non-zero port is
	// ambiguous — the runtime silently prefers Listener.Addr().
	// Warn so the caller notices the discard at config time rather
	// than observing a mismatched port in logs. Allow "<host>:0"
	// (pick-any-port) since it's a common pattern when the caller
	// intentionally delegates port selection to the pre-bound listener.
	if c.Listener != nil && c.Addr != "" && c.Addr != ":8080" {
		if _, port, splitErr := net.SplitHostPort(c.Addr); splitErr == nil && port != "0" {
			if lnAddr := c.Listener.Addr().String(); lnAddr != c.Addr {
				errs = append(errs, fmt.Errorf(
					"ambiguous configuration: Addr=%q but Listener is bound to %q; the explicit Addr will be discarded",
					c.Addr, lnAddr))
			}
		}
	}

	return errs
}

// WithDefaults returns a copy of Config with zero-value fields set to sensible
// defaults.
//
// It is idempotent: WithDefaults(WithDefaults(c)) resolves to the same values
// as WithDefaults(c). That matters because normalisation runs more than once
// on every start — Server.doPrepare normalises, then the engine constructor
// normalises again (adaptive a third time, before building its sub-engine) —
// and the negative "disabled" sentinels (ReadTimeout, ReadHeaderTimeout,
// WriteTimeout, IdleTimeout, MaxRequestBodySize) are carried internally as 0,
// which is also the "unset" input. See the defaulted field (celeris#594).
func (c Config) WithDefaults() Config {
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.Engine.IsDefault() {
		c.Engine = defaultEngine()
	}
	// Resolve h2c-upgrade default. Auto protocol (including the implicit
	// default) enables h2c upgrade; HTTP1/H2C don't unless the caller
	// explicitly set EnableH2Upgrade=true before calling WithDefaults.
	// Callers who want upgrade disabled on Auto must go through the root
	// celeris.Config path where EnableH2Upgrade is a *bool.
	wasAutoOrDefault := c.Protocol.IsDefault() || c.Protocol == engine.Auto
	if c.Protocol.IsDefault() {
		c.Protocol = engine.Auto
	}
	if wasAutoOrDefault && !c.EnableH2Upgrade {
		c.EnableH2Upgrade = true
	}
	if c.MaxFrameSize == 0 {
		// 1 MiB matches golang.org/x/net/http2's defaultMaxReadFrameSize
		// and fasthttp2 / hertz. RFC 7540 §4.2 permits up to 16 MiB. The
		// 16 KiB default previously rejected 32 KiB+ DATA frames from
		// clients that pre-negotiate their send size (Go std http2
		// client, loadgen, browser upload flows over H2).
		c.MaxFrameSize = 1 << 20
	}
	if c.InitialWindowSize == 0 {
		// 1 MiB matches golang.org/x/net/http2 and fasthttp2: a 64 KiB +
		// one-byte body POST would stall on the default 65 535-byte
		// window because the server's WINDOW_UPDATE lands only after it
		// finishes reading the full body. RFC 7540 allows up to 2^31-1.
		c.InitialWindowSize = 1 << 20
	}
	if c.MaxConcurrentStreams == 0 {
		c.MaxConcurrentStreams = 100
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = 16 << 20
	}
	switch {
	case c.MaxRequestBodySize < 0:
		c.MaxRequestBodySize = 0 // -1 → unlimited; 0 internally means unlimited
	case c.MaxRequestBodySize == 0 && !c.defaulted:
		c.MaxRequestBodySize = 100 << 20 // 100 MB
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	// Read/Write defaults. Previous 300s was too permissive for
	// a latency-focused engine — a slow-loris client could hold a
	// worker M / fd for 5 minutes before eviction. 60s matches
	// nginx's client_header_timeout / client_body_timeout and
	// covers legitimate slow-network cases. Users who need longer
	// (streaming uploads, big downloads) should set explicit values.
	c.ReadTimeout = resolveTimeout(c.ReadTimeout, 60*time.Second, c.defaulted)
	// ReadHeaderTimeout default: 10s. Short enough to defeat slow-
	// loris (whose canonical pattern is one byte every few hundred ms
	// for tens of seconds), long enough that legitimate proxies +
	// satellite clients still complete header reads. Mirrors nginx's
	// client_header_timeout default of 60s/10s and Go's
	// http.Server.ReadHeaderTimeout convention.
	c.ReadHeaderTimeout = resolveTimeout(c.ReadHeaderTimeout, 10*time.Second, c.defaulted)
	c.WriteTimeout = resolveTimeout(c.WriteTimeout, 60*time.Second, c.defaulted)
	c.IdleTimeout = resolveTimeout(c.IdleTimeout, 600*time.Second, c.defaulted)
	c.defaulted = true
	return c
}

// resolveTimeout applies one timeout field's sentinel rules exactly once.
//
// A negative value is the documented "no timeout" sentinel and becomes 0,
// the internal "disabled" encoding every consumer already tests with `> 0`.
// A zero means "unset" only on the first pass: once normalised is true, 0 is
// a disabled timeout this function must leave alone, otherwise the second
// WithDefaults (in the engine constructor) reinstates the default over an
// explicitly disabled timeout — celeris#594. Any positive value is kept
// verbatim on every pass, so N stays N.
func resolveTimeout(v, def time.Duration, normalised bool) time.Duration {
	switch {
	case v < 0:
		return 0 // -1 → no timeout
	case v == 0 && !normalised:
		return def
	default:
		return v
	}
}
