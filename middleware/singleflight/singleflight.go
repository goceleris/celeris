package singleflight

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/handoff"
)

type call struct {
	// done is closed by the leader when it has published the response. It
	// is made by the first waiter that joins, under g.mu, so a leader with
	// no waiter never makes one; the leader reads it under the same mutex
	// where it reads waiters, after it left the map, so no waiter can join
	// later. Waiters select on it and their request context directly.
	done chan struct{}
	// waiters counts concurrent requests that joined this leader's
	// in-flight state. Waiter goroutines increment under the group
	// mutex (before waiting on done); the leader reads
	// it under the same mutex at delete time, so count captures every
	// waiter that will ever read from this entry. A zero count lets
	// the leader skip body/header deep-copies — the work only matters
	// if someone else is going to read it back.
	waiters  atomic.Int32
	status   int
	headers  [][2]string
	body     []byte
	ct       string
	err      error
	panicVal any
	// rerun is set when the leader's handler failed because the leader's own
	// request context ended (its client went away, its deadline passed): that
	// error is not the waiters', and each waiter runs its own handler
	// instead of taking the leader's response (celeris#888 item 26).
	rerun bool
	// vary holds, for each request header the leader's response names in
	// Vary, the header and the leader's value for it (a copy); varyAll is
	// set when the response says Vary: *. A waiter whose request differs
	// from the leader's in one of them runs its own handler (celeris#912).
	vary    [][2]string
	varyAll bool
}

type group struct {
	mu sync.Mutex
	m  map[string]*call
}

// callPool recycles *call entries whose leader finished with zero
// waiters — the common single-flight miss. Entries with waiters
// accumulate captured body/headers and are released to GC when the
// last waiter drops its reference.
var callPool = sync.Pool{New: func() any { return &call{} }}

// testHookWaiterJoined fires (under g.mu, right after entry.waiters.Add)
// when a request joins an in-flight leader as a waiter. Nil in production;
// tests use it to synchronize deterministically on waiter-joined events
// instead of sleeping. Cost in production: one nil check per coalesced
// request.
var testHookWaiterJoined func()

func acquireCall() *call {
	c := callPool.Get().(*call)
	c.status = 0
	c.headers = c.headers[:0]
	c.body = c.body[:0]
	c.ct = ""
	c.err = nil
	c.panicVal = nil
	c.rerun = false
	c.done = nil
	c.vary = c.vary[:0]
	c.varyAll = false
	c.waiters.Store(0)
	return c
}

// New creates a singleflight middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	// A custom KeyFunc cannot be assumed to hold the validators: a
	// conditional request is then never coalesced. The default key holds them.
	customKey := cfg.KeyFunc != nil
	cfg = applyDefaults(cfg)
	cfg.validate()

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	keyFunc := cfg.KeyFunc

	g := &group{m: make(map[string]*call)}

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}
		// A request with a Range header is not coalesced: its response (a
		// 206 part) answers that Range, which the key does not include, so a
		// leader's part would be handed to requests for the whole
		// representation, and a ranged request waiting on a full one gets
		// more than it asked for (celeris#832).
		if c.Header("range") != "" {
			return c.Next()
		}
		var key string
		if customKey {
			// A conditional request is not coalesced under a custom KeyFunc
			// either: its response (a 304 or a 412) answers its validator,
			// which that key may not include (celeris#912).
			if hasPrecondition(c) {
				return c.Next()
			}
			key = keyFunc(c)
		} else {
			// The default key includes the validators, so GET and HEAD
			// requests that send the same ones are coalesced (celeris#888
			// item 23). A conditional write never is: its handler is the
			// update, and a request that took another's response instead
			// would lose it and never get its own 412.
			var conditional bool
			key, conditional = defaultKey(c)
			if conditional && !safeToCoalesce(c.Method()) {
				return c.Next()
			}
		}

		g.mu.Lock()
		if entry, ok := g.m[key]; ok {
			// Waiter path: another request is already in-flight for this key.
			// Increment the waiter count BEFORE releasing the mutex so the
			// leader's delete-time check sees it. Any waiter that reaches
			// this branch pairs with the leader's post-Next capture.
			entry.waiters.Add(1)
			if entry.done == nil {
				entry.done = make(chan struct{})
			}
			done := entry.done
			if testHookWaiterJoined != nil {
				testHookWaiterJoined()
			}
			g.mu.Unlock()
			// Race the leader's done channel against the request context so
			// a timeout middleware (or upstream client cancel) can preempt
			// the wait instead of blocking until the leader returns. Without
			// this, timeout(preemptive) → singleflight(waiter) deadlocks
			// when the leader exceeds the deadline because the timeout
			// goroutine would sit in the wait with no way out. The wait is a
			// select on the one channel the leader closes: no goroutine per
			// waiter (celeris#888 item 25).
			select {
			case <-done:
			case <-c.Context().Done():
				return c.Context().Err()
			}

			if entry.panicVal != nil {
				panic(entry.panicVal)
			}

			// The leader's own context ended and its handler said so: that
			// error is the leader's client's, and this request's client may
			// be there still. Run this request's own handler.
			if entry.rerun {
				return c.Next()
			}

			// The leader's response says it depends on a request header
			// this request sends differently (an Accept-Encoding that
			// compress negotiated, for one), and the key did not tell the
			// two apart: this request runs its own handler (celeris#912).
			if !entry.sameVariant(c) {
				return c.Next()
			}

			c.Abort()
			c.SetResponseHeaders(entry.headers)
			c.AddHeader("x-singleflight", "HIT")
			if entry.ct == "" {
				_ = c.NoContent(entry.status)
			} else {
				_ = c.Blob(entry.status, entry.ct, entry.body)
			}
			return entry.err
		}

		// Leader path: first request for this key.
		entry := acquireCall()
		g.m[key] = entry
		g.mu.Unlock()

		c.BufferResponse()

		var panicVal any
		var handlerErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicVal = r
				}
			}()
			handlerErr = c.Next()
		}()

		// Remove from map BEFORE closing done so new requests after it create
		// fresh entries. Reading entry.waiters (and done) under the same mutex
		// that gates waiter increments gives a consistent count of everyone
		// who will read from this entry; capture is a no-op when nobody's
		// waiting (the common single-flight miss).
		g.mu.Lock()
		delete(g.m, key)
		numWaiters := entry.waiters.Load()
		done := entry.done
		g.mu.Unlock()

		// The leader's own context ended and the handler's error says so.
		if numWaiters > 0 && handlerErr != nil && panicVal == nil {
			if cctx := c.Context(); cctx != nil {
				if cerr := cctx.Err(); cerr != nil && errors.Is(handlerErr, cerr) {
					entry.rerun = true
				}
			}
		}

		if numWaiters > 0 && !entry.rerun {
			// Capture response state (deep copy) only when a waiter will
			// consume it.
			entry.status = c.ResponseStatus()
			body := c.ResponseBody()
			if len(body) > 0 {
				entry.body = append([]byte(nil), body...)
			}
			entry.headers, entry.ct = ownHeaders(c.ResponseHeaders(), c.ResponseContentType())
			entry.captureVary(c)
			// The error and the panic value can hold the leader's request
			// strings too (errors.New(c.Param("id")), panic(c.Header("x"))),
			// which a waiter formats after the leader has returned: hand the
			// waiters copies (see handoff). The leader keeps its own.
			entry.err = handoff.Error(handlerErr)
			entry.panicVal = handoff.Panic(panicVal)
		}
		if done != nil {
			close(done)
		}

		// Pool-reuse the entry when no waiter referenced it. Entries seen
		// by waiters stay live until the last reader drops them (the
		// waiter path reads entry.body/headers/etc. after done closes); GC
		// reclaims them then. Returning those to the pool would be a
		// use-after-free.
		if numWaiters == 0 {
			callPool.Put(entry)
		}

		if panicVal != nil {
			panic(panicVal)
		}

		if ferr := c.FlushResponse(); ferr != nil && handlerErr == nil {
			handlerErr = ferr
		}
		return handlerErr
	}
}

// hasPrecondition reports a request that carries one of the four
// preconditions (If-None-Match and If-Modified-Since give a 304, If-Match and
// If-Unmodified-Since a 412, celeris#912). If-Range applies only with a Range.
func hasPrecondition(c *celeris.Context) bool {
	for _, v := range keyValidators {
		if c.Header(v.header) != "" {
			return true
		}
	}
	return false
}

// safeToCoalesce reports a method whose conditional requests the default key
// may coalesce: GET and HEAD, whose handler only reads, so a request that
// sends the leader's validator gets the leader's answer. For any other method
// a precondition marks a write guarded by it (a PUT with If-Match).
func safeToCoalesce(method string) bool { return method == "GET" || method == "HEAD" }

// captureVary records, for each request header the leader's response names
// in Vary (entry.headers, the waiters' copy), the leader's value for it, in
// canonical form (see canonicalAppend). The values are copied: on epoll and
// io_uring they are views of the leader connection's receive buffer, and a
// waiter compares them after the leader has returned.
func (e *call) captureVary(c *celeris.Context) {
	for _, h := range e.headers {
		if h[0] != "vary" {
			continue
		}
		for name := range strings.SplitSeq(h[1], ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			switch name {
			case "":
			case "*":
				e.varyAll = true
			default:
				e.vary = append(e.vary, [2]string{name, canonicalValue(name, c.Header(name))})
			}
		}
	}
}

// sameVariant reports whether the waiter c sends the leader's value
// (canonically equal, see canonicalAppend) for every request header the
// leader's response varies on, so the leader's response is the one c's own
// request would get.
func (e *call) sameVariant(c *celeris.Context) bool {
	if e.varyAll {
		return false
	}
	for _, v := range e.vary {
		if !sameCanonical(v[0], v[1], c.Header(v[0])) {
			return false
		}
	}
	return true
}

// ownHeaders returns a copy of the leader's response headers and content
// type for its waiters, the strings included; the copies share one
// allocation.
//
// The header values can be request strings: middleware echo request headers
// into the response (requestid's X-Request-Id, cors's
// Access-Control-Allow-Origin), and on epoll and io_uring those are views of
// the leader connection's receive buffer. The leader returns once it has
// handed the entry over, and the engine receives the connection's next
// request into that buffer (or, once the connection closes, gives it to
// another connection), while a waiter may serialize the headers later, for
// example when an outer middleware buffers its response (celeris#732).
func ownHeaders(hdrs [][2]string, ct string) ([][2]string, string) {
	n := len(ct)
	for _, h := range hdrs {
		n += len(h[0]) + len(h[1])
	}
	var b strings.Builder
	b.Grow(n)
	for _, h := range hdrs {
		b.WriteString(h[0])
		b.WriteString(h[1])
	}
	b.WriteString(ct)
	rest := b.String()
	var out [][2]string
	if len(hdrs) > 0 {
		out = make([][2]string, len(hdrs))
		for i, h := range hdrs {
			k, v := len(h[0]), len(h[1])
			out[i] = [2]string{rest[:k], rest[k : k+v]}
			rest = rest[k+v:]
		}
	}
	return out, rest
}
