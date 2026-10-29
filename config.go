package celeris

import (
	"log/slog"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/observe"
)

// Protocol represents the HTTP protocol version.
type Protocol engine.Protocol

const (
	// Auto enables automatic protocol detection between HTTP/1.1 and H2C (default).
	Auto Protocol = Protocol(engine.Auto)
	// HTTP1 selects HTTP/1.1 only.
	HTTP1 Protocol = Protocol(engine.HTTP1)
	// H2C selects HTTP/2 cleartext (h2c) only.
	H2C Protocol = Protocol(engine.H2C)
)

// String returns the protocol name.
func (p Protocol) String() string { return engine.Protocol(p).String() }

// EngineType identifies which I/O engine implementation is in use.
type EngineType engine.EngineType

const (
	// Adaptive dynamically switches between Epoll and IOUring based on load (default on Linux).
	Adaptive EngineType = EngineType(engine.Adaptive)
	// Epoll uses Linux edge-triggered epoll for I/O (Linux only).
	Epoll EngineType = EngineType(engine.Epoll)
	// IOUring uses Linux io_uring for asynchronous I/O (Linux 5.19+ required;
	// on an older kernel the engine fails to start, celeris#682).
	IOUring EngineType = EngineType(engine.IOUring)
	// Std uses Go's net/http standard library server (all platforms).
	Std EngineType = EngineType(engine.Std)
)

// String returns the engine type name.
func (t EngineType) String() string { return engine.EngineType(t).String() }

// WorkloadHint is an OPTIONAL declaration of expected steady-state concurrency,
// used only by the Adaptive engine's start-engine decision. Because connections
// cannot migrate between epoll and io_uring, the START engine decides keep-alive
// throughput — and the concurrency is unknowable when the server binds. This
// hint is the ONLY way to make Adaptive START on io_uring; without it Adaptive
// starts on epoll (best for the ramp-from-zero / low-concurrency / latency case)
// and promotes new connections to io_uring under sustained high load.
type WorkloadHint resource.WorkloadHint

const (
	// WorkloadUnspecified (default) → start epoll, promote under load.
	WorkloadUnspecified WorkloadHint = WorkloadHint(resource.WorkloadUnspecified)
	// WorkloadLowConcurrency → thin/latency-sensitive traffic; stay epoll.
	WorkloadLowConcurrency WorkloadHint = WorkloadHint(resource.WorkloadLowConcurrency)
	// WorkloadHighConcurrency → many h1 keep-alive conns/worker; start io_uring
	// (when the kernel and RLIMIT_MEMLOCK allow it).
	WorkloadHighConcurrency WorkloadHint = WorkloadHint(resource.WorkloadHighConcurrency)
)

// DeriveMemoryLimit returns a generous soft heap ceiling for a server with
// `workers` I/O workers — max(256 MiB, workers*32 MiB) — for use as
// [Config.MemoryLimitBytes]. Sized HIGH on purpose: the goal is to clip the
// connection-ramp balloon, not to run the heap tight. Never applied
// implicitly; pass it explicitly.
func DeriveMemoryLimit(workers int) int64 {
	return resource.DeriveMemoryLimit(workers)
}

// Config holds the public server configuration.
//
// # Response memory
//
// A response is held whole in memory until it is sent, except for a stream
// after [Context.Detach] (see [Context.StreamWriter]), and a single handler
// can stage a large one: a [Context.Blob] of any size, a [Context.File] of up
// to 100 MB. The limits below apply on the native engines (epoll, io_uring,
// adaptive) to what a slow client can leave unsent across requests and
// writes, not to one response. An HTTP/2 connection queues up to 64 MiB of
// unsent response data, past which a write is refused. An HTTP/1.1 request is
// not served while more than 4 MiB of responses on its connection are unsent,
// and the connection is closed once they have gone out; the 4 MiB is checked
// when the next request arrives, not per write, so it does not cut a response
// short. A detached connection (WebSocket, SSE) may queue 64 MiB, and over
// HTTP/1.1 a StreamWriter that is not detached is cut at 64 MiB.
//
// A [Context.File] response is limited to 100 MB. It is sent with sendfile(2),
// and not read into memory, only on epoll (not io_uring, std or HTTP/2), for
// an HTTP/1.1 response of at least 16 KiB, on a server with no async handlers
// (neither [Config.AsyncHandlers] nor a route marked [Route.Async]), and when
// no middleware buffers or captures the response ([Context.BufferResponse],
// [Context.CaptureResponse]). Any other File response is read whole into
// memory, as is one that a response pipelined behind it makes the engine copy
// into the write buffer, with no cap of its own. The std engine leaves
// response buffering to net/http.
type Config struct {
	// Addr is the TCP address to listen on (e.g. ":8080"). A port of 0 asks
	// for an ephemeral port, as in [net.Listen]: the engine picks it once and
	// every worker serves on that one port, which [Server.Addr] reports. The
	// epoll and io_uring engines keep the port bound, to a socket that never
	// listens, while accepting is paused and until they stop, so it stays the
	// same across a pause and resume of accepting. Adaptive (the default on
	// Linux) does not support [Server.PauseAccept]. It holds the port of any
	// address, not only port 0, from when it is built until its Listen returns
	// or the server begins to shut down. That includes a moment in which an
	// engine switch leaves no sub-engine listening, which can only happen
	// where the switch gives up waiting for the incoming engine's listener
	// (with [Config.DisableDeferAccept] the outgoing engine's listen sockets
	// close at once). It takes no hold for a listener handed to
	// [Server.StartWithListener]. While sockets that an earlier listener left
	// on the port have no SO_REUSEPORT (a net/http server's TIME_WAIT ends,
	// say), the hold it can take is the weaker one: it keeps out the sockets
	// that do not set SO_REUSEADDR, not those that do (net.Listen's do).
	Addr string
	// Protocol is the HTTP protocol version (default Auto — auto-detect H1/H2).
	Protocol Protocol
	// Engine is the I/O engine (default Adaptive on Linux, Std elsewhere).
	Engine EngineType

	// Workers is the number of I/O worker goroutines (default GOMAXPROCS).
	//
	// On Linux the native engines (epoll, io_uring) pin each loop to one CPU
	// the process is allowed to use. On a host whose allowed CPUs differ in
	// capacity (arm64 big.LITTLE) the default is at most one loop per CPU that
	// is not little, min(GOMAXPROCS, non-little CPUs): 8 loops on a 12-CPU host
	// with 4 little CPUs, which is fewer than GOMAXPROCS. Every one is pinned:
	// a loop left unpinned drifted onto the little CPUs and a loop pinned to
	// one stalled its connections. A Workers set above the non-little CPU
	// count is honoured and never left unpinned: the extra loops wrap
	// round-robin over the non-little CPUs, each pinned, so the CPUs that get a
	// second loop (4 of 8 for Workers 12) take about twice the connections of
	// the others, which get one. On io_uring a count is still capped by
	// RLIMIT_MEMLOCK. The loops started are Metrics.Workers of
	// [Server.EngineInfo] (on the adaptive engine, the sum over its two
	// engines), and the engine logs one line saying how many it started and
	// why. [Config.MaxConns] is per loop, so the server-wide limit follows the
	// count. Measured on one big.LITTLE host (arm64, 8 big and 4 little CPUs,
	// the NIC interrupt pinned to a big CPU; two dispatches, 35 s windows with
	// 5 repetitions per cell, 90 s windows with 4 at io_uring with 512
	// connections), against celeris v1.5.11 (all 12 loops pinned, 4 of them on
	// the little CPUs): 1.43 times the
	// requests per second at io_uring with 256 connections, 1.09 times at
	// adaptive with 128 connections, 1.51 times at io_uring with 512
	// connections, 1.04 times for epoll with 4 KiB POST bodies, and 0.95
	// times for epoll with 512 connections. The behaviour before this change
	// (4 floating loops) served those POST bodies at 0.47 times v1.5.11 in an
	// earlier run. The tail costs something: for epoll with 4 KiB POST bodies
	// the median p99.99 was 99 ms against 48 ms for v1.5.11, and at io_uring
	// with 512 connections a 90 s run had a median of 61 requests of 1 s or
	// more and its longest took 1.0 to 2.5 s, against 664 and 12 to 21 s for
	// v1.5.11. A Workers above the non-little CPU count is wrapped over the
	// pinned big CPUs; it ran once, in an earlier run (35 s windows, 3 repetitions), and showed a worse tail
	// than the default at io_uring with 512 connections (0.10% to 0.20% of
	// requests at about 208 ms, maxima of 2.1 to 2.2 s in two of three cells,
	// against 0.01% to 0.02% and 0.6 to 1.1 s for the default).
	//
	// With Workers unset on that kind of host, and a default count of at least one
	// loop for each non-little CPU (a lower GOMAXPROCS, such as the default under
	// a Kubernetes CPU limit, is planned as above: 7 default loops are 0, 1, 6, 7,
	// 8, 9 and 10, more than the 6 loops of 8), the loops also stay off the
	// non-little CPUs that service the busy interrupt vectors of the NIC port that
	// received the traffic (celeris#1036 item 1): the port that took at least 60%
	// of the packets received since boot, and at least a million, by the physical
	// ports behind the listen address, as read from /sys/class/net and
	// /proc/interrupts when the engine starts (vectors with at least 1000
	// interrupts since boot count as busy). At most a quarter of the non-little
	// CPUs go (2 of 8, so 6 loops), the count is never raised, a CPU that also
	// services a busy vector of another port keeps its loop, and at least one busy
	// vector of a port with at least 10% of the packets pooled over the listener's
	// ports (a choice, not measured) must stay on a CPU that keeps its loop: with
	// one port, or with a data NIC beside a management NIC that carries less, the
	// plan is the one without the rule. Nothing changes either when a file cannot
	// be read, when a port has a million packets and no busy vector, when the load
	// is spread over more CPUs than that quarter, or when a busy vector of another
	// port sits on a CPU that runs no loop. A Workers you set is planned as above,
	// whatever the NIC does. The engine's Info line names the CPUs, the port and
	// its share.
	//
	// Validated on one cluster host (msr1, arm64 big.LITTLE; probatorium runs
	// 39355553598 and 39367858847): the rule planned the loops 0, 1, 7, 9, 10 and
	// 11 by itself and matched the hand-pinned set within 1% requests per second
	// on io256, ad128, ep512 and post4k; against v1.5.11 io256 gained 35%, ad128
	// 5%, io512 45%, with the io512 maximum at 34 ms (v1.5.11: 14.1 s) and post4k
	// p99.99 at 3.4 ms (v1.5.11: 48 ms; throughput within 1% of the hand-pinned
	// set). The cost: ep512 throughput is about 0.845 times that of v1.5.11 (15%
	// lower; the 8 loops alone: 0.948 times). The pre-registered test came out
	// FAIL by its letter on the io256 p99.99 (5.59 times the hand-pinned set at
	// the medians; overlapping ranges, 3.2 to 32.8 ms against 3.6 to 28.3 ms). A
	// re-run of that cell on one commit (probatorium run 39396052154, 8
	// repetitions per arm) had the rule plan the same six loops by itself, with
	// 0.995 times the requests per second and a p99.99 median of 16.8 ms against
	// 6.0 ms for the same loops forced by a knob (2.83 times; 6 of 8 against 3 of
	// 8 repetitions over 10 ms); the one-sided exact Mann-Whitney test does not
	// reject (p=0.080) and the registered outcome is NOISE-ONLY, but at 8
	// repetitions its power is low and a modest p99.99 tail cost at io256 in the
	// 10 to 30 ms range is not excluded. The rule is fitted to that one host after
	// its data was seen; its counts are since boot (a fresh boot steps aside, a
	// wrapped 32-bit interrupt counter can hide a vector), a LAG rehash that moves
	// the traffic leaves the plan until the next start, a wildcard listener pools
	// every physical port that is up, ports that share all their vectors (mlx4)
	// never let the rule act, io_uring's RLIMIT_MEMLOCK cap keeps the first loops
	// of the plan in CPU order and can drop the CPU the rule meant to keep (a
	// memlock of 4 gives 0, 1, 7 and 9), and the adaptive engine's io_uring
	// standby is planned when it first switches, from the counts of that moment,
	// so it can pick another loop set than the primary.
	//
	// On such a host, once its loops have started, the engine also finds the CPUs
	// that service the interrupt vectors of the listener's NIC (the vectors that
	// carry traffic: at least 1000 interrupts and 5% of the NICs' since boot) and
	// logs one Warn when a serviced CPU of the allowed set runs no loop and is
	// either a little CPU or a big one while no vector found has a loop beside
	// it (the premise of the rule above: CPUs 6 and 8 of that host run no loop
	// and are fine while CPU 1 keeps one beside IRQ 167). It only reports: it
	// adds no loop, moves no loop and moves no interrupt, and it does not name
	// the CPUs the rule above left free on purpose. At that host's standing
	// state nothing is logged, with Workers 4 nothing is logged, and with the
	// vector of the other port on little CPU 4, the boot placement (the rule
	// steps aside), the Warn names CPU 4. On one bonded Realtek r8127 arm64
	// big.LITTLE host, with a TX-completion interrupt serviced by a CPU without a
	// celeris loop, whole-port TX silences of about 200 ms were seen, and they
	// were not seen after the interrupt was moved onto a CPU with a loop (one
	// host, one observation); an automatic extra loop on the little CPU was
	// tried and rejected because it raised tail latency on other workloads.
	// Moving the interrupt is the host operator's call (README, "NIC interrupts
	// on big.LITTLE hosts"). Hosts whose CPUs are alike are unchanged and read no
	// NIC file (README, "CPU pinning").
	Workers int

	// WorkloadHint optionally declares the expected steady-state concurrency.
	// It only affects the Adaptive engine's start-engine choice (see WorkloadHint):
	// the zero value starts on epoll; WorkloadHighConcurrency starts on io_uring
	// when the kernel + memlock allow.
	WorkloadHint WorkloadHint

	// ReadTimeout is the max duration for reading the entire request.
	// Zero uses the default (60s). Set to -1 for no timeout.
	//
	// On the native engines (epoll, io_uring, adaptive) it is measured from the
	// connection's last read (on io_uring also from the start and the completion
	// of its sends). On epoll a response the kernel did not take whole is
	// bounded by WriteTimeout's minimum rate alone, from the first sweep that
	// finds it parked until it ends (see WriteTimeout): ReadTimeout and
	// IdleTimeout run again from its end, and, where the socket reports no
	// acknowledged bytes, from the last flush that took bytes. A request
	// pipelined behind such a response is bounded by ReadHeaderTimeout for its
	// headers and by ReadTimeout only once the response has been sent; the
	// overrun check of an HTTP/1.1 async handler is suspended the same way.
	//
	// HTTP/2 on the native engines: each request is bounded by ReadTimeout from
	// its HEADERS, absolute, as net/http's is. A request that is not whole by
	// then (its body, or its trailers, still to come) is answered 408 and its
	// stream is reset with RST_STREAM(NO_ERROR); the connection and its other
	// streams go on. A DATA frame, or a PING, does not move the deadline, so an
	// HTTP/2 upload has to fit in ReadTimeout (as on std; there is no per-route
	// override), where an HTTP/1.1 body is bounded by the time since the last
	// read. A request without a body has no deadline.
	//
	// The connection's own clock (the last read) does not close an HTTP/2
	// connection that has a stream open whose request is whole (a response
	// waiting for the client's flow-control window is bounded by WriteTimeout,
	// see there, and no longer takes the exemption away): a stream the server is
	// writing to (an SSE stream) outlives a client that sends no frame. A stream
	// whose handler never answers is exempt for as long as it holds the stream,
	// where an HTTP/1.1 async handler that overruns ReadTimeout is closed. When
	// the last such stream ends the connection is kept for the timeout from the
	// sweep that last saw it. With no stream open, a silent connection is closed
	// at ReadTimeout or IdleTimeout from the client's last frame, and a client
	// that sends only PINGs, SETTINGS or connection WINDOW_UPDATEs is closed at
	// IdleTimeout (alone, as net/http's idle timer) from the last stream
	// activity, which those frames do not move. A response the client does not
	// take, from a client that keeps sending frames, is not bounded by ReadTimeout.
	ReadTimeout time.Duration
	// ReadHeaderTimeout caps the read of just the request line + headers
	// (separately from the request body, which ReadTimeout covers). This
	// is the canonical slow-loris defence: clients that drip headers one
	// byte at a time get killed in seconds instead of holding a worker
	// + listener-backlog slot for the full ReadTimeout window. Zero uses
	// the default (10s); -1 disables.
	//
	// The std engine wires this to http.Server.ReadHeaderTimeout; the
	// iouring/epoll engines enforce the same budget inside their H1
	// header read loop. On both it also counts for a connection whose
	// protocol is not detected yet (Protocol Auto, or H2C with
	// EnableH2Upgrade): a client that sends nothing, a few bytes of a request
	// line, or a dribble of the HTTP/2 preface is closed at the deadline, and
	// detecting the protocol does not start the budget again, so the first
	// request is bounded as a whole. The budget counts from the engine's
	// accept, which with TCP_DEFER_ACCEPT (see DisableDeferAccept) is the
	// connection's first byte or about a second after its SYN, whichever
	// comes first; std counts from the handshake.
	//
	// On HTTP/2 (epoll, io_uring, adaptive) it also bounds a header block: one
	// that begins with a HEADERS frame without END_HEADERS and is not complete this
	// long after that frame ends the connection with GOAWAY(ENHANCE_YOUR_CALM).
	ReadHeaderTimeout time.Duration
	// WriteTimeout is the max duration for writing the response; what it
	// bounds differs by engine, as the paragraphs below set out.
	// Zero uses the default (60s). Set to -1 for no timeout.
	//
	// A detached stream (see [Context.Detach]; the SSE middleware detaches)
	// is not cut at ReadTimeout or WriteTimeout as a whole on any engine, so
	// an SSE stream stays open for as long as its client keeps reading,
	// without setting WriteTimeout to -1. The native engines (epoll, io_uring,
	// adaptive) apply ReadTimeout and IdleTimeout to a detached connection
	// not at all, and WriteTimeout as "no progress for WriteTimeout": a
	// detached connection with bytes queued for its peer, which takes none of
	// them for WriteTimeout, is closed (the stream's context ends and its
	// OnDisconnect runs), whatever the middleware's idle deadline or the last
	// read of the connection. A peer that reads, however slowly, and a
	// connection with nothing queued are not closed (there is no minimum
	// rate; an HTTP/2 stream whose client lets its flow-control window run out
	// is bounded by the minimum rate described below); -1 sets no such bound. The close comes between about 3/4
	// and 5/4 of WriteTimeout after the peer last took a byte (its progress
	// is sampled every quarter of WriteTimeout; a SEND completion restarts the
	// clock at once), plus the engine's sweep interval (up to about 0.1 s on
	// epoll, up to 2 s on io_uring; a WriteTimeout shorter than that can
	// double). An h2c stream is not covered yet (celeris#884).
	// On the std engine net/http arms WriteTimeout as one deadline for the
	// whole response and Detach clears it for that request (ReadTimeout never
	// cuts a stream there: net/http has stopped it by the time the handler
	// runs).
	//
	// On std, WriteTimeout then bounds a detached stream's writes one at a
	// time: a Write or Flush that has not finished within WriteTimeout fails
	// (for an SSE event, which is small, that means the client has stopped
	// reading: a client that stays connected and reads nothing ends the stream
	// that way; one very large write to a client that reads too slowly for it
	// fails the same; -1 sets no such bound), and when ShutdownTimeout runs
	// out the writes of the streams still running fail. A response that is not detached, such as
	// a large download or a chunked body written with [Context.StreamWriter]
	// without Detach, stays under WriteTimeout as an absolute deadline for the
	// whole response: raise it or set -1 for such routes. On the epoll engine
	// (and adaptive while it runs on epoll) WriteTimeout bounds a response that
	// is not detached differently: it is a minimum rate on the bytes the client
	// acknowledges, with no elapsed-time ceiling. A response the kernel did not
	// accept whole is queued in the engine ("parked"); the engine's sweep reads
	// how many bytes the client has acknowledged (tcp_info) and holds the
	// connection another WriteTimeout if the client acknowledged at least 64 KiB
	// in the last one (a floor of 64 KiB per WriteTimeout: 64 KiB/s at 1s,
	// about 1 KiB/s at the 60s default), and closes it if it did not. A client
	// that reads at or above the floor is never cut, however long the response
	// takes, and one that reads steadily between two EPOLLOUT edges is not
	// mistaken for a stalled one: a download completes whatever IdleTimeout is.
	// A client that takes less, in whatever pieces (nothing, a byte at a time,
	// a half send queue per WriteTimeout), is closed at the end of the first
	// window, between WriteTimeout and WriteTimeout plus two sweeps after the
	// response parked (the first sweep takes the baseline, the window ends at
	// the first sweep a WriteTimeout later); one that stops after a window it
	// passed, within two WriteTimeouts plus two sweeps after its last byte. A
	// client that takes exactly 64 KiB per window holds the connection, and the
	// part of the response not yet sent, for as many windows as the response has
	// 64 KiB (a response of S bytes: S/64 KiB times WriteTimeout, 64 minutes
	// for 4 MiB at 60s; a smaller WriteTimeout raises the floor), so size
	// WriteTimeout with that in mind. WriteTimeout -1 is no write timeout at
	// any rate. What the client sends is not progress: a pipelined request or
	// an HTTP/2 PING does not hold a parked response. The times above hold
	// with ReadHeaderTimeout set (the default): the engine's 25 ms sweep timer
	// is armed only then, and without it a stalled response can outlast them by
	// seconds (measured: WriteTimeout 1s, ReadHeaderTimeout -1, closed at 5 s,
	// on main too).
	//
	// While the rule has a baseline for a parked response (from the first sweep
	// that finds it parked), it is the only bound on the connection:
	// ReadTimeout and IdleTimeout do not apply, so a client at or above the
	// floor is not cut by either whatever they are next to WriteTimeout (the
	// defaults are 60s, 600s and 60s), and one that stops is closed by the rule
	// within the times above. They apply again from the end of the response. A
	// socket that does not report acknowledged bytes (not TCP, or a TCP stack
	// that leaves bytes_acked at 0) has the flush clock alone, "no flush took
	// bytes for WriteTimeout", and ReadTimeout and IdleTimeout run from the
	// last flush that took bytes there, as before the rule. The rule covers an
	// HTTP/2 response parked on the socket too. A response that waits for the
	// client's HTTP/2 flow-control window is not parked (nothing is queued on
	// the socket); WriteTimeout bounds its stream in its own right, as a
	// minimum rate, on the native engines: in each WriteTimeout window a stream
	// that holds buffered bytes must have had at least 64 KiB of them (or all
	// it held, if less) sent, released by the client's WINDOW_UPDATEs, or it is
	// reset with RST_STREAM(INTERNAL_ERROR), as net/http resets a stream at its
	// write deadline. There is no ceiling, where std's deadline is absolute: a
	// stream that releases at least that on average, lagging the rate by no
	// more than four windows' worth (surplus released in one update carries to
	// the next windows, and the bytes in flight when the stream stalled cover its
	// first), is not cut however long its response takes. A PING or a
	// WINDOW_UPDATE of a byte is not progress. A client that updates in big steps
	// (nghttp2 and curl, at half the window) is fine while the gap between its
	// updates stays under about five WriteTimeouts: half the window divided by the
	// rate it reads at. When the connection as a whole releases at least 64 KiB
	// in a window, a stream that takes less (parallel downloads share the
	// client's socket) is held while it takes half its fair share of it, and for
	// three windows in a row when it takes less: a download queued behind its
	// siblings' bytes in the client's socket is cut where the bytes ahead of it
	// take longer than three WriteTimeouts to read (N streams of W bytes in flight
	// on a link of L bytes/s: N x W / L), so a server whose clients are on slow
	// links should set WriteTimeout to 5s or more. When the connection itself
	// releases less than 64 KiB, the first window is the verdict. A stream the
	// client never grants is reset after 1+k windows alone, or 4+k with siblings
	// that keep the connection moving, k being the number of whole 64 KiB in the
	// bytes in flight when it stalled (about the peer's initial window), at most
	// four: 1 and 4 WriteTimeouts for the default 64 KiB window, 5 and 8 for a
	// window of 256 KiB or more; a one-byte WINDOW_UPDATE dribbler is reset in the
	// 2nd window of the default window and in the 5th of a larger one. A stream
	// whose own window has room but whose bytes wait for the connection's is
	// credited with what the whole connection released. While such a stream
	// lives, ReadTimeout and IdleTimeout do not close the connection
	// (WriteTimeout is its bound), so a silent client's unread response, and the
	// buffer it pins, lives for up to 5 WriteTimeouts (1, with the default window)
	// where it used to be dropped with the whole connection at ReadTimeout. A
	// handler blocked waiting for window keeps its own, absolute, bound of
	// WriteTimeout from the start of the wait, whatever the client reads: parallel
	// downloads on one connection whose responses exceed the connection's 4 MiB
	// output budget can lose the later ones to it. A request error read while a
	// response is queued (a malformed request) puts the connection on the
	// closing drain below, which counts each byte the client acknowledges and
	// has no floor: a client that sends one and then takes a few bytes per
	// WriteTimeout holds the connection, a known limit that is not changed here.
	// io_uring is unchanged: it has no rate, and measures WriteTimeout from
	// the last read, the start of a send from an idle connection or a
	// completed send (a client that completes one send per WriteTimeout, or
	// sends another request, restarts it), not from the request's headers as
	// std does. On io_uring a response whose
	// unsent remainder is under 64 KiB, to a client that stops reading, on an
	// otherwise idle worker, can outlast WriteTimeout (measured with async
	// responses of 96 to 144 KiB; the larger and the sync sizes are closed).
	// On epoll, a response parked behind a handler that holds the connection's
	// dispatch lock for longer than WriteTimeout can be closed at WriteTimeout
	// even though nothing is queued for it (a narrow pipelined-handler case).
	// A response that ends the connection (Connection: close, a request error,
	// a refused write) has a drain of its own on both native engines: the connection is cut
	// once the client has taken no byte for WriteTimeout (epoll) or
	// max(5s, WriteTimeout) (io_uring), and for 5 s on either when WriteTimeout
	// is -1. A byte taken is a flush or send completion that moved the
	// response, or bytes the client acknowledged (tcp_info, sampled about every
	// quarter of that bound), so a client that reads steadily, however slowly,
	// is not cut, and holds the connection and the unsent response for as long
	// as it does. Each byte taken restarts that clock: there is no absolute
	// limit on such a connection.
	//
	// With -1, an HTTP/2 response that has to wait for the client's
	// flow-control window has no deadline either on epoll, io_uring and
	// adaptive: a handler that the connection's buffering budget (4 MiB) makes
	// wait stays blocked until the client grants window (WINDOW_UPDATE) or the
	// connection closes, so a stalled HTTP/2 client can hold a stream (and its
	// handler) for as long as it keeps the connection open. A synchronous
	// handler that waits for such a handler, for example one that needs a lock
	// the waiting handler holds across its response write, holds its event
	// loop; if that loop also owns the slow client's connection, it never
	// sees the WINDOW_UPDATE or the close, the wait never ends, and Shutdown
	// cannot stop the server. std recovers once the client closes. Keep
	// WriteTimeout finite when handlers share locks across response writes.
	WriteTimeout time.Duration
	// IdleTimeout is the max duration a keep-alive connection may be idle.
	// Zero uses the default (600s). Set to -1 for no timeout. On an HTTP/2
	// connection it runs from the last stream activity while no stream is open
	// (PING, SETTINGS and WINDOW_UPDATE frames do not move it) and is not
	// applied while a stream the server owes an answer for is open; see
	// ReadTimeout. A connection that still has output to send (in its buffers or
	// the kernel's send queue) is not idle while the output goes out: the period
	// restarts, and the connection is closed (GOAWAY with NO_ERROR where it can
	// still be sent, then the close; the end of the output may be lost) at the end
	// of a period in which the peer acknowledged no more than the control frames
	// (PING and SETTINGS acknowledgements, WINDOW_UPDATEs) the server sent it, as a
	// client that has stopped reading and one that pings faster than its round
	// trip leave it; so is one that has acknowledged nothing at all. A client that
	// reads, however slowly, acknowledges more. Where the bytes the peer has
	// acknowledged cannot be read (a host whose TCP stack does not report them, a
	// failed getsockopt) the period is always extended. A frame for a stream the server reset moves the period only for a
	// request that expired at ReadTimeout, for the 2 x ReadTimeout its late DATA may
	// take. On epoll, for a response parked on the socket, see WriteTimeout.
	IdleTimeout time.Duration
	// ShutdownTimeout is the deadline of the graceful shutdown that cancelling
	// the context of StartWithContext or StartWithListenerAndContext starts
	// (default 30s): one deadline for the drain of in-flight requests and then
	// the OnShutdown hooks, which run with what is left of it.
	ShutdownTimeout time.Duration

	// MaxFormSize is the maximum memory used for multipart form parsing
	// (default 32 MB). Set to -1 to disable the limit. It applies to a body
	// the engine has already accepted, so it is also bounded by
	// [Config.MaxRequestBodySize] (default 4 MiB): a multipart body over that
	// is refused before the handler runs.
	MaxFormSize int64

	// MaxRequestBodySize is the maximum allowed request body size in bytes
	// across all protocols (H1, H2, bridge). 0 uses the default (4 MiB; it
	// was 100 MB before v1.6.0). Set to -1 to disable the limit (unlimited).
	//
	// The engine buffers the whole body in memory before the handler runs, so
	// the limit times the number of requests in flight is what clients can
	// make the server hold (each HTTP/2 stream is a request, up to
	// MaxConcurrentStreams per connection). The limit is server-wide: set it
	// to the largest body any route accepts, and cap the other routes lower
	// with the middleware/bodylimit package; treat -1 as a choice to accept
	// the memory. A body over the limit is refused before the handler runs:
	// with 413 on HTTP/1.1 and on HTTP/2 under the std engine, with
	// RST_STREAM on HTTP/2 under the epoll, io_uring and adaptive engines
	// (which currently also end the connection). A multipart or form body is
	// a request body like any other, so with the default the default
	// [Config.MaxFormSize] (32 MB) is never reached; raise both for larger
	// multipart uploads.
	MaxRequestBodySize int64

	// MaxConcurrentStreams limits simultaneous H2 streams per connection (default 100).
	MaxConcurrentStreams uint32
	// MaxFrameSize is the max H2 frame payload size (default 16384, range 16384-16777215).
	MaxFrameSize uint32
	// InitialWindowSize is the H2 initial stream window size (default 65535).
	InitialWindowSize uint32
	// MaxHeaderBytes is the max header block size (default 16 MB, min 4096).
	// Only the std engine applies it (net/http's Server.MaxHeaderBytes).
	// The native engines (epoll, io_uring, adaptive) ignore it: they cap an
	// HTTP/1 request head at a fixed 64 KiB, and they refuse an HTTP/2 request
	// whose header list is over a fixed 64 KiB (the sum, over its fields, of
	// len(name) + len(value) + 32, as RFC 9113 section 6.5.2 counts it) with
	// RST_STREAM(ENHANCE_YOUR_CALM) on that stream, where std answers 431.
	MaxHeaderBytes int

	// DisableKeepAlive disables HTTP keep-alive; each request gets its own connection.
	DisableKeepAlive bool
	// DisableDeferAccept turns TCP_DEFER_ACCEPT off on the epoll and io_uring
	// engines' listen sockets. Default false: the option stays on.
	//
	// [Server.PauseAccept] carries a client that has connected but not yet
	// sent its request across the pause either way. With the option on it
	// does so by lingering: it clears the option on each listen socket and
	// keeps it open for about 1.5 s before closing it, so it blocks that
	// long. Set this field for a pause that closes the listen sockets at
	// once.
	//
	// What the option saves: while it is on, the kernel keeps a connection
	// that has completed its handshake but sent no data out of the accept
	// queue, so a client that sends its request at once is accepted with
	// the request already queued, and the engine saves a wakeup per new
	// connection. A client that stays silent is accepted about one second
	// after its SYN and is then subject to ReadHeaderTimeout like any other.
	// Turning the option off gives up that wakeup, a cost on connection
	// churn only; keep-alive traffic, where one accept is amortised over
	// many requests, does not see it.
	DisableDeferAccept bool
	// BufferSize is the per-connection I/O buffer size in bytes (0 = engine default).
	BufferSize int
	// SocketRecvBuf sets SO_RCVBUF for accepted connections (0 = OS default).
	SocketRecvBuf int
	// SocketSendBuf sets SO_SNDBUF for accepted connections (0 = OS default).
	SocketSendBuf int
	// MaxConns is the max simultaneous connections per worker (0 = unlimited);
	// the server-wide limit is MaxConns times the loops started, see [Config.Workers].
	MaxConns int

	// MemoryLimitBytes is an OPTIONAL soft heap ceiling applied via
	// runtime/debug.SetMemoryLimit at Start. 0 = unset: celeris does not
	// touch the process GC, so embedders keep full control. When >0, the GC
	// collects before the heap balloons during a connection-ramp burst
	// (under the default GOGC=100 that ramp spike is the dominant peak-RSS
	// contributor), trading a few extra GC cycles during the ramp for a
	// lower high-water mark; steady RSS sits far below the limit so steady
	// throughput is unaffected. NOTE: SetMemoryLimit is PROCESS-GLOBAL, so
	// this is opt-in only — set it when celeris owns the process (e.g. a
	// dedicated server binary). See [DeriveMemoryLimit] for a sized default.
	MemoryLimitBytes int64

	// DisableMetrics disables the built-in metrics collector. When true,
	// [Server.Collector] returns nil and per-request metric recording is skipped.
	// Default false (metrics enabled).
	DisableMetrics bool

	// AsyncHandlers dispatches HTTP handlers to spawned goroutines instead of
	// running them inline on the engine's LockOSThread'd worker. Enabling
	// this is the right choice when handlers do blocking I/O (DB drivers,
	// external HTTP calls, file reads): the worker returns to epoll_wait /
	// io_uring_enter while the handler blocks, so the per-worker serialization
	// ceiling (NumWorkers × 1/RTT) is replaced by goroutine-per-connection
	// parallelism, matching net/http's concurrency model.
	//
	// When AsyncHandlers is set, celeris drivers opened WithEngine(srv) auto-
	// select their direct net.Conn path (Go netpoll parks handler Gs on
	// EPOLLIN without blocking an M), validated on MSR1 at celeris-epoll +
	// celerismc jumping from 64k → 105k rps.
	//
	// The cost on pure-CPU handlers is a goroutine spawn per request (~100ns)
	// plus scheduler overhead — measured regression on a static-response
	// bench is ~3–5%. Keep AsyncHandlers false for latency-critical CPU-only
	// workloads; enable it for any workload that touches a DB, cache, or
	// upstream service.
	//
	// A handler that runs inline must not call runtime.Goexit (t.FailNow,
	// t.Fatal and t.SkipNow in a test's handler do, and so do some
	// libraries): on the native engines it runs on the event-loop goroutine,
	// and a Goexit ends that loop for good, leaving its connections and its
	// SO_REUSEPORT listener unserved. The engine logs an Error and cannot
	// recover; see [Route.Sync]. The global middleware and the NotFound or
	// MethodNotAllowed handler of a request no route matches run inline too
	// (see below), so they must not call it either. An [Route.Async] route's
	// handler does not run on the loop, so a Goexit there leaves the loop
	// serving: on HTTP/1.1 the native engines close that connection and log an
	// Error; on HTTP/2 the stream gets no answer and nothing is logged (see
	// [Route.Async]).
	//
	// A handler that runs inline runs on a thread the native engines (epoll,
	// io_uring) pinned to one CPU, and a process it starts inherits that pin:
	// os/exec clones the calling thread, the kernel copies its CPU mask into
	// the child, and the mask survives execve and passes to the child's own
	// children. A handler that shells out to ffmpeg, git or an image converter
	// would run it on a single CPU. Go has no per-process CPU field in
	// os/exec's SysProcAttr, so celeris cannot undo it inside the call. Start
	// such a process from a goroutine of its own and wait for it, or mark the
	// route [Route.Async] (or its group, [RouteGroup.Async]): a goroutine
	// never runs on the locked loop thread, so its children get the process's
	// mask, and an Async route's handler runs on one. Setting AsyncHandlers
	// is not enough: a route that only inherits it runs inline, on the loop
	// thread, until a timed run of it blocks, and returns to inline when the
	// promotion expires. A thread that cgo code called from an inline handler
	// creates (pthread_create) inherits the pin the same way. Threads Go
	// starts itself do not. On a host whose CPUs differ in capacity (arm64
	// big.LITTLE) the loops pin to the non-little CPUs only (README, "CPU
	// pinning").
	//
	// AsyncHandlers is the SERVER-LEVEL default. Individual routes and
	// groups can override it per handler with [Route.Async] /
	// [RouteGroup.Async] (most-specific wins: route > group > this
	// default), so a server with mostly CPU routes + a few DB routes can
	// keep this false and mark just the DB routes .Async() / .UsesDriver(),
	// or set this true and mark hot CPU routes .Sync(). On HTTP/2 the
	// override is honored per-stream (sync routes run inline on the event
	// loop, async routes dispatch to the worker pool).
	//
	// A request no route matches (a 404, a 405, the automatic OPTIONS
	// answer) runs the global middleware ([Server.Use]) and any NotFound /
	// MethodNotAllowed handler. With this set it is dispatched like a route
	// that inherits this default: inline until a timed run of that chain
	// blocks, then async. All unmatched requests share that one state, and a
	// run of fast ones settles it as it settles a route; from then on an
	// unmatched request whose chain blocks runs inline on the engine worker,
	// holding every connection on it, unless it is the one timed after a
	// settle re-open. With this unset it always runs inline, even when
	// routes are .Async(). So a global middleware that can block (a remote
	// session store, an auth upstream, pprof's profile) can hold a worker
	// for unmatched requests; mount it on .Async() routes where that matters.
	//
	// DRIVERS: celeris drivers opened WithEngine(srv) pick their netpoll-park
	// fast path from the server's EFFECTIVE async state — true when this flag
	// is set OR any route is .Async(). So "keep this false + mark DB routes
	// .Async()/.UsesDriver()" selects the fast driver path too, PROVIDED the
	// driver is opened AFTER those routes are registered (the effective state
	// is read at driver construction); otherwise set this true. Setting this
	// true also enables the adaptive safety net that auto-promotes any unmarked
	// handler slower than ~300µs, at a small learning-phase cost that settles to
	// near zero for static routes: a settled route is re-timed for one handler
	// run every adaptiveSettleTTL (celeris#592) — measured at one timed run per
	// concurrently-executing inline handler per tick, ~120 ns each — so a
	// handler whose backend turns slow after it settled is still promoted
	// within that bound instead of blocking an engine worker forever.
	//
	// Default: false.
	AsyncHandlers bool

	// OnExpectContinue is called when an H1 request contains "Expect: 100-continue".
	// If the callback returns false, the server responds with 417 Expectation Failed
	// and skips reading the body. If nil, the server always sends 100 Continue.
	// The method, the path and the header strings are request strings, valid only
	// during the call (on epoll and io_uring, views of the receive buffer; see the
	// package doc): copy one with [strings.Clone] before anything keeps it.
	// Several Cookie lines are one field joined with "; ", as the handler sees
	// them.
	//
	// On the native engines the callback runs on the event-loop goroutine,
	// except on a connection already promoted to async dispatch, where it runs
	// on that connection's goroutine. It must not call runtime.Goexit: on the
	// loop that ends the loop for good, and the engine only logs an Error (see
	// [Route.Sync]).
	OnExpectContinue func(method, path string, headers [][2]string) bool

	// OnConnect is called when a new connection is accepted. The addr is the
	// remote peer address. Must be fast — blocks the event loop.
	//
	// It runs on the event-loop goroutine on the native engines, whether the
	// connection's routes are [Route.Async] or not, so it must not call
	// runtime.Goexit: that ends the loop for good and the engine only logs an
	// Error (see [Route.Sync]).
	OnConnect func(addr string)
	// OnDisconnect is called when a connection is closed. The addr is the
	// remote peer address. Must be fast — blocks the event loop.
	//
	// The same contract as [Config.OnConnect]: it runs on the event-loop
	// goroutine on the native engines and must not call runtime.Goexit.
	OnDisconnect func(addr string)

	// TrustedProxies is a list of trusted proxy CIDR ranges (e.g., "10.0.0.0/8",
	// "172.16.0.0/12", "192.168.0.0/16"). When set, ClientIP() only trusts
	// X-Forwarded-For hops from these networks. When empty, ClientIP() trusts
	// all proxy headers (legacy behavior).
	TrustedProxies []string

	// Logger is the structured logger (default slog.Default()).
	Logger *slog.Logger

	// EnableH2Upgrade controls whether the server honors RFC 7540 §3.2
	// "HTTP/1.1 Upgrade: h2c" requests, promoting an HTTP/1 connection to
	// cleartext HTTP/2 on the original request's handler (dispatched on
	// stream 1). Resolution follows three cases:
	//   - nil (default): inferred from Protocol — enabled for Auto,
	//     disabled for H2C (clients already speak H2 directly) and for
	//     HTTP1 (the connection stays HTTP/1.1 unless true asks for the
	//     upgrade).
	//   - non-nil true: force enabled. Useful to opt into upgrade on
	//     Protocol=H2C or HTTP1 for clients that prefer to negotiate.
	//   - non-nil false: force disabled, even on Protocol=Auto. Useful when
	//     the engine intentionally only serves HTTP/1, or to refuse the
	//     upgrade handshake (a request-smuggling surface behind some
	//     proxies): a request carrying Upgrade: h2c is then served as
	//     plain HTTP/1.1. Prior-knowledge h2c on Auto is not affected.
	//
	// All four engines (Std, Epoll, IOUring, Adaptive) decide whether a
	// well-formed Upgrade: h2c request (Connection lists Upgrade and
	// HTTP2-Settings, Upgrade is exactly h2c, exactly one HTTP2-Settings
	// whose value decodes) is upgraded the same way for every Protocol and
	// every value:
	//
	//	Protocol  nil   &false  &true
	//	Auto      101   no      101
	//	H2C       no    no      101
	//	HTTP1     no    no      101
	//
	// "no" is no upgrade: on Auto and HTTP1 the request is served as plain
	// HTTP/1.1. On H2C the listener is HTTP/2: Epoll, IOUring and Adaptive
	// answer such a request with an HTTP/2 SETTINGS frame, and Std, which
	// also serves plain HTTP/1.1 on an H2C listener, serves it as HTTP/1.1.
	// Prior-knowledge h2c (a connection that opens with the HTTP/2 preface)
	// is served on Auto and H2C whatever this says, and never on HTTP1.
	//
	// The HTTP2-Settings header follows RFC 7540 section 3.2.1 on all four
	// engines: a request with exactly one such header is upgraded when its
	// value is a base64url SETTINGS payload (a whole number of 6-byte
	// entries; an empty value is a payload with none), and a request whose
	// header is missing, repeated or not such a payload is not upgraded: it
	// is served as plain HTTP/1.1, never answered with an error status.
	EnableH2Upgrade *bool

	// DisableAutoHEAD turns off the router's automatic HEAD answer
	// (celeris#421). By default a HEAD request to a path that has a GET route
	// and no HEAD route runs the GET route (RFC 9110 §9.3.2: HEAD is GET
	// without the content; the engine sends the headers and drops the body),
	// and the path's Allow lists HEAD. When true, the router no longer
	// answers HEAD with a GET route: a HEAD request that no HEAD route (or
	// middleware, below) answers is answered like a method the path does not
	// support, 405 Method Not Allowed with an Allow header that does not list
	// HEAD (or 404 if the path has no route), running the
	// [Server.MethodNotAllowed] handler. What still answers HEAD: a route
	// registered with [Server.HEAD] or [Server.Any] (which registers HEAD),
	// and middleware that answers it itself (healthcheck, swagger, static,
	// anything in a [Server.Pre] or [Server.Use] chain that writes the
	// response). Use it to keep a GET handler with a side effect, or a
	// streaming one, from running for HEAD requests. Default: false (HEAD is
	// answered by the GET route).
	DisableAutoHEAD bool

	// DisableAutoOPTIONS turns off the router's automatic OPTIONS answer
	// (celeris#421). By default an OPTIONS request to a path that has routes
	// and no OPTIONS route is answered 200 with an Allow header and
	// Content-Length: 0 (RFC 9110 §9.3.7) after the global middleware
	// ([Server.Use]) has run, and the path's Allow lists OPTIONS. When true,
	// the router no longer answers it: an OPTIONS request that no OPTIONS
	// route (also through [Server.Any]) or middleware, below, answers is
	// answered like a method the path does not support (405 with an Allow
	// header that does not list OPTIONS, or 404 if the path has no route).
	// The global middleware still runs for it, so a CORS middleware
	// installed with [Server.Use] still answers its preflight, and so does
	// any other middleware that answers OPTIONS itself. Default: false.
	DisableAutoOPTIONS bool
}

// EngineMetrics is a point-in-time snapshot of engine-level performance counters.
type EngineMetrics = observe.EngineMetrics

// EngineInfo provides read-only information about the running engine.
type EngineInfo struct {
	// Type identifies the active I/O engine (IOUring, Epoll, Adaptive, or Std).
	Type EngineType
	// Metrics is a point-in-time snapshot of engine-level performance counters.
	Metrics EngineMetrics
}

func (c Config) toResourceConfig() resource.Config {
	rc := resource.Config{
		Addr:                 c.Addr,
		Protocol:             engine.Protocol(c.Protocol),
		Engine:               engine.EngineType(c.Engine),
		ReadTimeout:          c.ReadTimeout,
		ReadHeaderTimeout:    c.ReadHeaderTimeout,
		WriteTimeout:         c.WriteTimeout,
		IdleTimeout:          c.IdleTimeout,
		MaxHeaderBytes:       c.MaxHeaderBytes,
		MaxConcurrentStreams: c.MaxConcurrentStreams,
		MaxFrameSize:         c.MaxFrameSize,
		InitialWindowSize:    c.InitialWindowSize,
		DisableKeepAlive:     c.DisableKeepAlive,
		DisableDeferAccept:   c.DisableDeferAccept,
		Logger:               c.Logger,
	}

	if c.Workers > 0 {
		rc.Resources.Workers = c.Workers
	}
	if c.BufferSize > 0 {
		rc.Resources.BufferSize = c.BufferSize
	}
	if c.SocketRecvBuf > 0 {
		rc.Resources.SocketRecv = c.SocketRecvBuf
	}
	if c.SocketSendBuf > 0 {
		rc.Resources.SocketSend = c.SocketSendBuf
	}
	if c.MaxConns > 0 {
		rc.Resources.MaxConns = c.MaxConns
	}
	rc.Resources.MemoryLimitBytes = c.MemoryLimitBytes
	rc.Resources.WorkloadHint = resource.WorkloadHint(c.WorkloadHint)

	rc.MaxRequestBodySize = c.MaxRequestBodySize
	rc.AsyncHandlers = c.AsyncHandlers
	rc.OnExpectContinue = c.OnExpectContinue
	rc.OnConnect = c.OnConnect
	rc.OnDisconnect = c.OnDisconnect

	// h2c upgrade resolution. Nil → protocol-dependent default (Auto → true,
	// HTTP1/H2C → false). Non-nil → user override honored verbatim. The
	// result goes through SetH2Upgrade so resource.Config.WithDefaults, which
	// cannot tell an explicit false from unset, leaves it alone: it used to
	// turn &false back into true on Auto (celeris#964).
	var enableH2Upgrade bool
	if c.EnableH2Upgrade != nil {
		enableH2Upgrade = *c.EnableH2Upgrade
	} else {
		p := engine.Protocol(c.Protocol)
		enableH2Upgrade = p.IsDefault() || p == engine.Auto
	}
	rc.SetH2Upgrade(enableH2Upgrade)

	return rc
}
