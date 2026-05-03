// Package chi registers go-chi/chi on top of net/http against the
// perfmatrix registry. Three flavours: H1, H2C, Auto.
package chi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	chiv5 "github.com/go-chi/chi/v5"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

const kind = "chi"

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

// Server is the chi implementation.
type Server struct {
	name     string
	mode     mode
	features servers.FeatureSet

	jsonSmall []byte
	json1K    []byte
	json64K   []byte

	mu     sync.Mutex
	router chiv5.Router
	srv    *http.Server
	ln     net.Listener

	// drivers holds driver clients (built by mountDriverHandlers) so
	// Stop can close them on teardown.
	drivers *driverState

	// mountedChain / mountedDriver guard against double-registration of
	// /chain/* and driver routes on repeated Start calls.
	mountedChain  bool
	mountedDriver bool
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
	s.router = chiv5.NewRouter()
	s.registerStatic()
	return s
}

// Name implements servers.Server.
func (s *Server) Name() string { return s.name }

// Kind implements servers.Server.
func (s *Server) Kind() string { return kind }

// Features implements servers.Server.
func (s *Server) Features() servers.FeatureSet { return s.features }

// Mount attaches an http.Handler under (method, path).
func (s *Server) Mount(method, path string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.router.Method(strings.ToUpper(method), path, h)
}

func (s *Server) registerStatic() {
	s.router.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Hello, World!"))
	})
	s.router.Get("/json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.jsonSmall)
	})
	s.router.Get("/json-1k", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.json1K)
	})
	s.router.Get("/json-64k", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.json64K)
	})
	s.router.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chiv5.URLParam(r, "id")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "User ID: %s", id)
	})
	s.router.Post("/upload", func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("OK"))
	})
}

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func drainBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, 32*1024)
	for {
		if _, err := r.Body.Read(buf); err != nil {
			return
		}
	}
}

// Start implements servers.Server.
func (s *Server) Start(ctx context.Context, svcs *services.Handles) (net.Listener, error) {
	_ = ctx
	// Routes are registered exactly once per Server via mount guards;
	// repeat Start calls rebuild driver clients but do not re-register
	// routes (chi's router panics on duplicate patterns).
	mountChainHandlers(s)
	mountDriverHandlers(s, svcs)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.ln = ln
	h2s := &http2.Server{MaxReadFrameSize: 1 << 20, IdleTimeout: 120 * time.Second}
	var handler http.Handler = s.router
	if s.mode == modeH2C || s.mode == modeAuto {
		handler = h2c.NewHandler(s.router, h2s)
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
	err := s.srv.Shutdown(ctx)
	s.shutdownDriverHandlers()
	return err
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
		panic(fmt.Sprintf("chi perfmatrix: build payload failed: %v", err))
	}
	return out
}

func init() {
	servers.Register(newServer("chi-h1", modeH1))
	servers.Register(newServer("chi-h2c", modeH2C))
	servers.Register(newServer("chi-auto", modeAuto))
}
