package singleflight

import (
	"sync"

	"github.com/goceleris/celeris"
)

type call struct {
	wg       sync.WaitGroup
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

		key := keyFunc(c)

		g.mu.Lock()
		if entry, ok := g.m[key]; ok {
			// Waiter path: another request is already in-flight for this key.
			g.mu.Unlock()
			entry.wg.Wait()

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
		entry := &call{}
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

		// Capture response state (deep copy).
		entry.status = c.ResponseStatus()
		entry.ct = c.ResponseContentType()
		body := c.ResponseBody()
		if len(body) > 0 {
			entry.body = append([]byte(nil), body...)
		}
		respHeaders := c.ResponseHeaders()
		if len(respHeaders) > 0 {
			entry.headers = make([][2]string, len(respHeaders))
			copy(entry.headers, respHeaders)
		}
		entry.panicVal = panicVal

		// Remove from map BEFORE wg.Done so new requests after Done create fresh entries.
		g.mu.Lock()
		delete(g.m, key)
		entry.wg.Done()
		g.mu.Unlock()

		if panicVal != nil {
			panic(panicVal)
		}

		handlerErr := entry.err
		if ferr := c.FlushResponse(); ferr != nil && handlerErr == nil {
			handlerErr = ferr
		}
		return handlerErr
	}
}
