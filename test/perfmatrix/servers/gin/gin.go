// Package gin registers gin-gonic/gin against the perfmatrix registry.
// Three flavours: H1, H2C, Auto.
package gin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	ginv1 "github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

const kind = "gin"

type payloadSmall struct {
	Message string `json:"message"`
	Server  string `json:"server"`
}

type mode int

const (
	modeH1 mode = iota
	modeH2C
	modeAuto
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func init() {
	ginv1.SetMode(ginv1.ReleaseMode)
	ginv1.DefaultWriter = discardWriter{}
	ginv1.DefaultErrorWriter = discardWriter{}
	servers.Register(newServer("gin-h1", modeH1))
	servers.Register(newServer("gin-h2c", modeH2C))
	servers.Register(newServer("gin-auto", modeAuto))
}

// Server is the gin implementation.
type Server struct {
	name     string
	mode     mode
	features servers.FeatureSet

	jsonSmall []byte
	json1K    []byte
	json64K   []byte

	mu     sync.Mutex
	engine *ginv1.Engine
	srv    *http.Server
	ln     net.Listener
}

func newServer(name string, m mode) *Server {
	s := &Server{name: name, mode: m}
	switch m {
	case modeH1:
		s.features = servers.FeatureSet{HTTP1: true, Drivers: true, Middleware: true}
	case modeH2C:
		s.features = servers.FeatureSet{HTTP2C: true, H2CUpgrade: true, Drivers: true, Middleware: true}
	case modeAuto:
		s.features = servers.FeatureSet{HTTP1: true, HTTP2C: true, Auto: true, H2CUpgrade: true, Drivers: true, Middleware: true}
	}
	small, _ := json.Marshal(payloadSmall{Message: "Hello, World!", Server: kind})
	s.jsonSmall = small
	s.json1K = buildJSONPayload(1024)
	s.json64K = buildJSONPayload(64 * 1024)
	s.engine = ginv1.New()
	s.registerStatic()
	return s
}

// Name implements servers.Server.
func (s *Server) Name() string { return s.name }

// Kind implements servers.Server.
func (s *Server) Kind() string { return kind }

// Features implements servers.Server.
func (s *Server) Features() servers.FeatureSet { return s.features }

// Mount attaches an http.Handler via gin.WrapH.
func (s *Server) Mount(method, path string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engine.Handle(strings.ToUpper(method), path, ginv1.WrapH(h))
}

func (s *Server) registerStatic() {
	s.engine.GET("/", func(c *ginv1.Context) {
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte("Hello, World!"))
	})
	s.engine.GET("/json", func(c *ginv1.Context) {
		c.Data(http.StatusOK, "application/json", s.jsonSmall)
	})
	s.engine.GET("/json-1k", func(c *ginv1.Context) {
		c.Data(http.StatusOK, "application/json", s.json1K)
	})
	s.engine.GET("/json-64k", func(c *ginv1.Context) {
		c.Data(http.StatusOK, "application/json", s.json64K)
	})
	s.engine.GET("/users/:id", func(c *ginv1.Context) {
		id := c.Param("id")
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(fmt.Sprintf("User ID: %s", id)))
	})
	s.engine.POST("/upload", func(c *ginv1.Context) {
		drainBody(c.Request)
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte("OK"))
	})
}

func drainBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	defer r.Body.Close()
	buf := make([]byte, 32*1024)
	for {
		if _, err := r.Body.Read(buf); err != nil {
			return
		}
	}
}

// Start implements servers.Server.
func (s *Server) Start(ctx context.Context, _ *services.Handles) (net.Listener, error) {
	_ = ctx
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.ln = ln

	h2s := &http2.Server{MaxReadFrameSize: 1 << 20, IdleTimeout: 120 * time.Second}
	var handler http.Handler = s.engine
	if s.mode == modeH2C || s.mode == modeAuto {
		handler = h2c.NewHandler(s.engine, h2s)
	}
	s.srv = &http.Server{
		Handler:     handler,
		IdleTimeout: 120 * time.Second,
	}
	if s.mode == modeAuto || s.mode == modeH2C {
		_ = http2.ConfigureServer(s.srv, h2s)
	}
	go func() { _ = s.srv.Serve(ln) }()
	return ln, nil
}

// Stop implements servers.Server.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// buildJSONPayload mirrors the celeris builder.
func buildJSONPayload(targetBytes int) []byte {
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
	}{Size: dataLen, Data: string(data)})
	if err != nil {
		panic(fmt.Sprintf("gin perfmatrix: build payload failed: %v", err))
	}
	return out
}
