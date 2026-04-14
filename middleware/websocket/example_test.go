package websocket_test

import (
	"fmt"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/websocket"
)

// Echo each message back to the client. The middleware automatically
// uses the engine-integrated path on epoll/io_uring (with TCP-level
// backpressure) and falls back to net/http Hijack on the std engine.
func ExampleNew() {
	s := celeris.New(celeris.Config{})

	s.GET("/ws", websocket.New(websocket.Config{
		EnableCompression: true,
		IdleTimeout:       60 * time.Second,
		Handler: func(c *websocket.Conn) {
			for {
				mt, data, err := c.ReadMessageReuse()
				if err != nil {
					return
				}
				if err := c.WriteMessage(mt, data); err != nil {
					return
				}
			}
		},
	}))

	fmt.Println("WebSocket echo handler installed at /ws")
	// Output: WebSocket echo handler installed at /ws
}

// Use OnConnect to authenticate and OnDisconnect to release resources.
func ExampleConfig_lifecycleHooks() {
	cfg := websocket.Config{
		OnConnect: func(c *websocket.Conn) error {
			// Reject unauthenticated peers — return non-nil to close the
			// connection before the Handler runs.
			if c.Subprotocol() != "v1.echo" {
				return fmt.Errorf("subprotocol required")
			}
			return nil
		},
		OnDisconnect: func(c *websocket.Conn) {
			// Release per-connection resources (DB handles, channels…).
			_ = c
		},
		Handler: func(c *websocket.Conn) {
			_ = c
		},
	}
	_ = websocket.New(cfg)
	fmt.Println("ok")
	// Output: ok
}

// Tune the engine-integrated backpressure watermarks. Default is
// 75% pause / 25% resume on a 256-chunk buffer; raise the pause point
// when the handler is bursty but generally fast.
func ExampleConfig_backpressure() {
	cfg := websocket.Config{
		MaxBackpressureBuffer: 1024,
		BackpressureHighPct:   90,
		BackpressureLowPct:    50,
		Handler: func(c *websocket.Conn) {
			_ = c
		},
	}
	_ = websocket.New(cfg)
	fmt.Println("ok")
	// Output: ok
}
