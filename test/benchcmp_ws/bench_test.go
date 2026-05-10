package benchcmp_ws

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	gorillaws "github.com/gorilla/websocket"

	"github.com/goceleris/celeris"
	celerisws "github.com/goceleris/celeris/middleware/websocket"
)

func startCelerisWS(b *testing.B, handler celerisws.Handler) (string, func()) {
	b.Helper()
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.GET("/ws", celerisws.New(celerisws.Config{Handler: handler}))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- s.StartWithListener(ln) }()
	return "ws://" + ln.Addr().String() + "/ws", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
		<-done
	}
}

func startRawWS(b *testing.B, handler func(*gorillaws.Conn)) (string, func()) {
	b.Helper()
	u := gorillaws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handler(c)
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	return "ws://" + ln.Addr().String() + "/ws", func() { srv.Close() }
}

func dial(b *testing.B, url string) *gorillaws.Conn {
	b.Helper()
	c, _, err := gorillaws.DefaultDialer.Dial(url, nil)
	if err != nil {
		b.Fatal(err)
	}
	return c
}

func BenchmarkWSUpgrade_Celeris(b *testing.B) {
	url, shutdown := startCelerisWS(b, func(c *celerisws.Conn) {})
	defer shutdown()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c := dial(b, url)
		c.Close()
	}
}

func BenchmarkWSUpgrade_RawGorilla(b *testing.B) {
	url, shutdown := startRawWS(b, func(c *gorillaws.Conn) { c.Close() })
	defer shutdown()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c := dial(b, url)
		c.Close()
	}
}

func BenchmarkWSEcho_Celeris(b *testing.B) {
	url, shutdown := startCelerisWS(b, func(c *celerisws.Conn) {
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteMessage(mt, msg)
		}
	})
	defer shutdown()
	conn := dial(b, url)
	defer conn.Close()
	p := []byte("hello websocket bench!")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		conn.WriteMessage(gorillaws.TextMessage, p)
		conn.ReadMessage()
	}
}

func BenchmarkWSEcho_RawGorilla(b *testing.B) {
	url, shutdown := startRawWS(b, func(c *gorillaws.Conn) {
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteMessage(mt, msg)
		}
	})
	defer shutdown()
	conn := dial(b, url)
	defer conn.Close()
	p := []byte("hello websocket bench!")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		conn.WriteMessage(gorillaws.TextMessage, p)
		conn.ReadMessage()
	}
}

func BenchmarkWSEchoLarge_Celeris(b *testing.B) {
	url, shutdown := startCelerisWS(b, func(c *celerisws.Conn) {
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteMessage(mt, msg)
		}
	})
	defer shutdown()
	conn := dial(b, url)
	defer conn.Close()
	p := make([]byte, 65536)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		conn.WriteMessage(gorillaws.BinaryMessage, p)
		conn.ReadMessage()
	}
}

func BenchmarkWSEchoLarge_RawGorilla(b *testing.B) {
	url, shutdown := startRawWS(b, func(c *gorillaws.Conn) {
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteMessage(mt, msg)
		}
	})
	defer shutdown()
	conn := dial(b, url)
	defer conn.Close()
	p := make([]byte, 65536)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		conn.WriteMessage(gorillaws.BinaryMessage, p)
		conn.ReadMessage()
	}
}

// startCelerisHubWS spins up a celeris server whose WebSocket handler
// registers each connection with the supplied Hub. Returns the URL and
// a teardown.
func startCelerisHubWS(b *testing.B, hub *celerisws.Hub) (string, func()) {
	return startCelerisWS(b, func(c *celerisws.Conn) {
		unreg := hub.Register(c)
		defer unreg()
		<-c.Context().Done()
	})
}

// startRawGorillaHubWS spins up the gorilla equivalent: a hand-rolled
// connection set + RWMutex broadcast loop, the pattern the celeris Hub
// is designed to replace.
type gorillaHub struct {
	mu    sync.RWMutex
	conns map[*gorillaws.Conn]struct{}
}

func (h *gorillaHub) register(c *gorillaws.Conn)   { h.mu.Lock(); h.conns[c] = struct{}{}; h.mu.Unlock() }
func (h *gorillaHub) unregister(c *gorillaws.Conn) { h.mu.Lock(); delete(h.conns, c); h.mu.Unlock() }
func (h *gorillaHub) broadcast(msg []byte) (int, error) {
	h.mu.RLock()
	conns := make([]*gorillaws.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	delivered := 0
	for _, c := range conns {
		if err := c.WriteMessage(gorillaws.TextMessage, msg); err == nil {
			delivered++
		}
	}
	return delivered, nil
}

func startGorillaHubServer(b *testing.B, hub *gorillaHub) (string, func()) {
	return startRawWS(b, func(c *gorillaws.Conn) {
		hub.register(c)
		defer hub.unregister(c)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})
}

func benchmarkHubBroadcast_Celeris(b *testing.B, n int) {
	hub := celerisws.NewHub(celerisws.HubConfig{})
	defer hub.Close()
	url, shutdown := startCelerisHubWS(b, hub)
	defer shutdown()
	conns := make([]*gorillaws.Conn, n)
	for i := range conns {
		conns[i] = dial(b, url)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for hub.Len() != n {
		time.Sleep(time.Millisecond)
	}
	pm, _ := celerisws.NewPreparedMessage(celerisws.OpText, []byte("payload"))
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = hub.BroadcastPrepared(pm)
	}
}

func benchmarkHubBroadcast_GorillaHandRolled(b *testing.B, n int) {
	hub := &gorillaHub{conns: make(map[*gorillaws.Conn]struct{})}
	url, shutdown := startGorillaHubServer(b, hub)
	defer shutdown()
	conns := make([]*gorillaws.Conn, n)
	for i := range conns {
		conns[i] = dial(b, url)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		l := len(hub.conns)
		hub.mu.RUnlock()
		if l == n {
			break
		}
		time.Sleep(time.Millisecond)
	}
	payload := []byte("payload")
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = hub.broadcast(payload)
	}
}

func BenchmarkHubBroadcast100_Celeris(b *testing.B)         { benchmarkHubBroadcast_Celeris(b, 100) }
func BenchmarkHubBroadcast100_GorillaHandRolled(b *testing.B) { benchmarkHubBroadcast_GorillaHandRolled(b, 100) }
