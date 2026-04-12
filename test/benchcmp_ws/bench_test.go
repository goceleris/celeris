package benchcmp_ws

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisws "github.com/goceleris/celeris/middleware/websocket"
	gorillaws "github.com/gorilla/websocket"
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
		if err != nil { return }
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
	if err != nil { b.Fatal(err) }
	return c
}

func BenchmarkWSUpgrade_Celeris(b *testing.B) {
	url, shutdown := startCelerisWS(b, func(c *celerisws.Conn) {})
	defer shutdown()
	b.ReportAllocs(); b.ResetTimer()
	for b.Loop() { c := dial(b, url); c.Close() }
}

func BenchmarkWSUpgrade_RawGorilla(b *testing.B) {
	url, shutdown := startRawWS(b, func(c *gorillaws.Conn) { c.Close() })
	defer shutdown()
	b.ReportAllocs(); b.ResetTimer()
	for b.Loop() { c := dial(b, url); c.Close() }
}

func BenchmarkWSEcho_Celeris(b *testing.B) {
	url, shutdown := startCelerisWS(b, func(c *celerisws.Conn) {
		for { mt, msg, err := c.ReadMessage(); if err != nil { return }; c.WriteMessage(mt, msg) }
	})
	defer shutdown()
	conn := dial(b, url); defer conn.Close()
	p := []byte("hello websocket bench!")
	b.ReportAllocs(); b.ResetTimer()
	for b.Loop() { conn.WriteMessage(gorillaws.TextMessage, p); conn.ReadMessage() }
}

func BenchmarkWSEcho_RawGorilla(b *testing.B) {
	url, shutdown := startRawWS(b, func(c *gorillaws.Conn) {
		defer c.Close()
		for { mt, msg, err := c.ReadMessage(); if err != nil { return }; c.WriteMessage(mt, msg) }
	})
	defer shutdown()
	conn := dial(b, url); defer conn.Close()
	p := []byte("hello websocket bench!")
	b.ReportAllocs(); b.ResetTimer()
	for b.Loop() { conn.WriteMessage(gorillaws.TextMessage, p); conn.ReadMessage() }
}

func BenchmarkWSEchoLarge_Celeris(b *testing.B) {
	url, shutdown := startCelerisWS(b, func(c *celerisws.Conn) {
		for { mt, msg, err := c.ReadMessage(); if err != nil { return }; c.WriteMessage(mt, msg) }
	})
	defer shutdown()
	conn := dial(b, url); defer conn.Close()
	p := make([]byte, 65536)
	b.ReportAllocs(); b.ResetTimer()
	for b.Loop() { conn.WriteMessage(gorillaws.BinaryMessage, p); conn.ReadMessage() }
}

func BenchmarkWSEchoLarge_RawGorilla(b *testing.B) {
	url, shutdown := startRawWS(b, func(c *gorillaws.Conn) {
		defer c.Close()
		for { mt, msg, err := c.ReadMessage(); if err != nil { return }; c.WriteMessage(mt, msg) }
	})
	defer shutdown()
	conn := dial(b, url); defer conn.Close()
	p := make([]byte, 65536)
	b.ReportAllocs(); b.ResetTimer()
	for b.Loop() { conn.WriteMessage(gorillaws.BinaryMessage, p); conn.ReadMessage() }
}
