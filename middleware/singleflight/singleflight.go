package singleflight

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris"
)

type call struct {
	wg sync.WaitGroup
	// waiters counts concurrent requests that joined this leader's
	// in-flight state. Waiter goroutines increment under the group
	// mutex (before registering on the WaitGroup); the leader reads
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
	c.waiters.Store(0)
	return c
}

// New creates a singleflight middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
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

		key := keyFunc(c)

		g.mu.Lock()
		if entry, ok := g.m[key]; ok {
			// Waiter path: another request is already in-flight for this key.
			// Increment the waiter count BEFORE releasing the mutex so the
			// leader's delete-time check sees it. Any waiter that reaches
			// this branch pairs with the leader's post-Next capture.
			entry.waiters.Add(1)
			if testHookWaiterJoined != nil {
				testHookWaiterJoined()
			}
			g.mu.Unlock()
			// Race the leader's WaitGroup against the request context so a
			// timeout middleware (or upstream client cancel) can preempt the
			// wait instead of blocking until the leader returns. Without
			// this, timeout(preemptive) → singleflight(waiter) deadlocks
			// when the leader exceeds the deadline because the timeout
			// goroutine sits inside wg.Wait() with no way out.
			waitDone := make(chan struct{})
			go func() {
				entry.wg.Wait()
				close(waitDone)
			}()
			select {
			case <-waitDone:
			case <-c.Context().Done():
				return c.Context().Err()
			}

			if entry.panicVal != nil {
				panic(entry.panicVal)
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
		entry.wg.Add(1)
		g.m[key] = entry
		g.mu.Unlock()

		c.BufferResponse()

		var panicVal any
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicVal = r
				}
			}()
			entry.err = c.Next()
		}()

		// Remove from map BEFORE wg.Done so new requests after Done create
		// fresh entries. Reading entry.waiters under the same mutex that
		// gates waiter increments gives a consistent count of everyone who
		// will read from this entry; capture is a no-op when nobody's
		// waiting (the common single-flight miss).
		g.mu.Lock()
		delete(g.m, key)
		numWaiters := entry.waiters.Load()
		g.mu.Unlock()

		if numWaiters > 0 {
			// Capture response state (deep copy) only when a waiter will
			// consume it.
			entry.status = c.ResponseStatus()
			body := c.ResponseBody()
			if len(body) > 0 {
				entry.body = append([]byte(nil), body...)
			}
			entry.headers, entry.ct = ownHeaders(c.ResponseHeaders(), c.ResponseContentType())
			entry.panicVal = panicVal
		}
		entry.wg.Done()

		// Snapshot any leader-side state BEFORE returning entry to the
		// pool — otherwise a concurrent acquireCall could race with the
		// reads below.
		handlerErr := entry.err

		// Pool-reuse the entry when no waiter referenced it. Entries seen
		// by waiters stay live until the last reader drops them (the
		// waiter path reads entry.body/headers/etc. after wg.Done); GC
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
