// Package iris registers kataras/iris v12 against the perfmatrix
// registry. Three flavours: H1, H2C, Auto.
package iris

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	irisv12 "github.com/kataras/iris/v12"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

const kind = "iris"

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

// Server is the iris v12 implementation. We run iris's Application as
// an http.Handler behind http.Server (+ optional h2c) so H1/H2C/Auto
// wiring matches stdhttp/chi/echo/gin.
type Server struct {
	name     string
	mode     mode
	features servers.FeatureSet

	jsonSmall []byte
	json1K    []byte
	json64K   []byte

	mu    sync.Mutex
	app   *irisv12.Application
	built bool
	srv   *http.Server
	ln    net.Listener
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
	s.app = irisv12.New()
	s.app.Configure(irisv12.WithOptimizations)
	s.registerStatic()
	return s
}

// Name implements servers.Server.
func (s *Server) Name() string { return s.name }

// Kind implements servers.Server.
func (s *Server) Kind() string { return kind }

// Features implements servers.Server.
func (s *Server) Features() servers.FeatureSet { return s.features }

// Mount attaches an http.Handler under (method, path). Bridges
// net/http to iris handlers.
func (s *Server) Mount(method, path string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wrapped := func(ctx irisv12.Context) {
		h.ServeHTTP(ctx.ResponseWriter(), ctx.Request())
	}
	s.app.Handle(strings.ToUpper(method), path, wrapped)
}

func (s *Server) registerStatic() {
	s.app.Get("/", func(ctx irisv12.Context) {
		ctx.ContentType("text/plain; charset=utf-8")
		_, _ = ctx.Write([]byte("Hello, World!"))
	})
	s.app.Get("/json", func(ctx irisv12.Context) {
		ctx.ContentType("application/json")
		_, _ = ctx.Write(s.jsonSmall)
	})
	s.app.Get("/json-1k", func(ctx irisv12.Context) {
		ctx.ContentType("application/json")
		_, _ = ctx.Write(s.json1K)
	})
	s.app.Get("/json-64k", func(ctx irisv12.Context) {
		ctx.ContentType("application/json")
		_, _ = ctx.Write(s.json64K)
	})
	s.app.Get("/users/{id}", func(ctx irisv12.Context) {
		id := ctx.Params().Get("id")
		ctx.ContentType("text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(ctx.ResponseWriter(), "User ID: %s", id)
	})
	s.app.Post("/upload", func(ctx irisv12.Context) {
		drainBody(ctx.Request())
		ctx.ContentType("text/plain; charset=utf-8")
		_, _ = ctx.Write([]byte("OK"))
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
	s.mu.Lock()
	if !s.built {
		if err := s.app.Build(); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("iris build: %w", err)
		}
		s.built = true
	}
	s.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.ln = ln

	// iris.Application embeds *router.Router which exposes ServeHTTP; use
	// it directly as an http.Handler.
	var handler http.Handler = s.app
	h2s := &http2.Server{MaxReadFrameSize: 1 << 20, IdleTimeout: 120 * time.Second}
	if s.mode == modeH2C || s.mode == modeAuto {
		handler = h2c.NewHandler(s.app, h2s)
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
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.srv.Shutdown(shutdownCtx)
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
		panic(fmt.Sprintf("iris perfmatrix: build payload failed: %v", err))
	}
	return out
}

func init() {
	servers.Register(newServer("iris-h1", modeH1))
	servers.Register(newServer("iris-h2c", modeH2C))
	servers.Register(newServer("iris-auto", modeAuto))
}
