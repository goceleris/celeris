package sse

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/goceleris/celeris"
)

// Handler is the function signature for SSE endpoint handlers.
// The Client is valid for the duration of the call. When Handler returns,
// the SSE stream is closed automatically.
type Handler func(client *Client)

// Client provides the API for sending SSE events to a connected client.
// It wraps a [celeris.StreamWriter] and manages event formatting, heartbeat,
// and disconnect detection.
type Client struct {
	sw          *celeris.StreamWriter
	ctx         context.Context
	cancel      context.CancelFunc
	lastEventID string
	buf         []byte
	mu          sync.Mutex
	closed      bool
}

// Send sends a complete SSE event. Thread-safe (serialized with heartbeat writes).
// Returns an error if the client has disconnected or the stream is closed.
func (c *Client) Send(e Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClientClosed
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}

	c.buf = formatEvent(c.buf, &e)
	if _, err := c.sw.Write(c.buf); err != nil {
		c.cancel()
		return err
	}
	if err := c.sw.Flush(); err != nil {
		c.cancel()
		return err
	}
	return nil
}

// SendData is a convenience for sending a data-only event.
// Equivalent to Send(Event{Data: data}).
func (c *Client) SendData(data string) error {
	return c.Send(Event{Data: data})
}

// SendComment sends a comment line. Useful for custom keep-alives.
func (c *Client) SendComment(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClientClosed
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}

	c.buf = formatComment(c.buf, text)
	if _, err := c.sw.Write(c.buf); err != nil {
		c.cancel()
		return err
	}
	if err := c.sw.Flush(); err != nil {
		c.cancel()
		return err
	}
	return nil
}

// Close closes the SSE stream. Safe to call multiple times.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.cancel()
	return c.sw.Close()
}

// Context returns a context.Context that is cancelled when the client
// disconnects or the stream is closed.
func (c *Client) Context() context.Context {
	return c.ctx
}

// LastEventID returns the value of the Last-Event-ID header sent by the
// client on reconnection. Returns empty string for initial connections.
func (c *Client) LastEventID() string {
	return c.lastEventID
}

const maxPooledBufSize = 4096

var clientPool = sync.Pool{New: func() any {
	return &Client{buf: make([]byte, 0, 256)}
}}

func acquireClient(ctx context.Context, sw *celeris.StreamWriter, cancel context.CancelFunc, lastEventID string) *Client {
	c := clientPool.Get().(*Client)
	c.sw = sw
	c.ctx = ctx
	c.cancel = cancel
	c.lastEventID = lastEventID
	c.buf = c.buf[:0]
	c.closed = false
	return c
}

func releaseClient(c *Client) {
	c.sw = nil
	c.ctx = nil
	c.cancel = nil
	c.lastEventID = ""
	if cap(c.buf) <= maxPooledBufSize {
		clientPool.Put(c)
	}
}

// New creates an SSE handler with the given config.
//
// Usage:
//
//	server.GET("/events", sse.New(sse.Config{
//	    Handler: func(client *sse.Client) {
//	        for msg := range messages {
//	            if err := client.Send(sse.Event{Data: msg}); err != nil {
//	                return
//	            }
//	        }
//	    },
//	}))
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	handler := cfg.Handler
	heartbeatInterval := cfg.HeartbeatInterval
	onConnect := cfg.OnConnect
	onDisconnect := cfg.OnDisconnect

	// Pre-format retry line if configured.
	var retryLine []byte
	if cfg.RetryInterval > 0 {
		retryLine = []byte("retry: " + strconv.Itoa(cfg.RetryInterval) + "\n\n")
	}

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	// SSE response headers. Shared across handlers; safe because both
	// the H1 and H2 StreamWriter.WriteHeader implementations copy values
	// out of this slice synchronously and do not retain a reference.
	// See TestSSEStreamWriterDoesNotRetainHeaders in sse_test.go for the
	// pinned regression check.
	sseHeaders := [][2]string{
		{"content-type", "text/event-stream"},
		{"cache-control", "no-cache"},
		{"x-accel-buffering", "no"},
	}

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		// Extract Last-Event-ID before Detach materializes headers.
		lastEventID := c.Header("last-event-id")

		sw := c.StreamWriter()
		if sw == nil {
			return celeris.NewHTTPError(500, "SSE requires streaming support")
		}

		// Run OnConnect before writing headers so rejection can return
		// a proper HTTP error code to the client.
		ctx, cancel := context.WithCancel(c.Context())
		client := acquireClient(ctx, sw, cancel, lastEventID)

		if onConnect != nil {
			if err := onConnect(c, client); err != nil {
				cancel()
				releaseClient(client)
				return err
			}
		}

		done := c.Detach()

		if err := sw.WriteHeader(200, sseHeaders); err != nil {
			cancel()
			releaseClient(client)
			done()
			return err
		}

		// heartbeatDone is closed by the heartbeat goroutine on exit so the
		// defer can wait for it before returning the Client to the pool.
		// Without this, the goroutine could read client.closed after a
		// future test reused the pooled Client (data race).
		var heartbeatDone chan struct{}

		defer func() {
			_ = client.Close()
			if heartbeatDone != nil {
				<-heartbeatDone
			}
			if onDisconnect != nil {
				onDisconnect(c, client)
			}
			releaseClient(client)
			done()
		}()

		// Send initial retry interval.
		if len(retryLine) > 0 {
			client.mu.Lock()
			_, err := sw.Write(retryLine)
			if err == nil {
				err = sw.Flush()
			}
			client.mu.Unlock()
			if err != nil {
				return nil
			}
		}

		// Start heartbeat goroutine.
		if heartbeatInterval > 0 {
			heartbeatDone = make(chan struct{})
			go func() {
				defer close(heartbeatDone)
				ticker := time.NewTicker(heartbeatInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						client.mu.Lock()
						if client.closed {
							client.mu.Unlock()
							return
						}
						_, err := sw.Write(heartbeatBytes)
						if err == nil {
							err = sw.Flush()
						}
						client.mu.Unlock()
						if err != nil {
							cancel()
							return
						}
					}
				}
			}()
		}

		handler(client)
		return nil
	}
}
