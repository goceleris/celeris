// Package stdhttp registers the standard library net/http server
// against the perfmatrix registry in three flavours: H1, H2C, and Auto
// (H1+H2C on a single listener using h2c.NewHandler).
package stdhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

const kind = "stdhttp"

// mode selects which HTTP protocol stack the Server exposes.
type mode int

const (
	modeH1 mode = iota
	modeH2C
	modeAuto
)

// Server is the net/http-based implementation.
type Server struct {
	name     string
	mode     mode
	features servers.FeatureSet

	jsonSmall []byte
	json1K    []byte
	json64K   []byte

	mu  sync.Mutex
	mux *http.ServeMux
	srv *http.Server
	ln  net.Listener
}

// newServer constructs a Server for the requested mode.
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
	s.mux = http.NewServeMux()
	s.registerStatic()
	return s
}

// Name implements servers.Server.
func (s *Server) Name() string { return s.name }

// Kind implements servers.Server.
func (s *Server) Kind() string { return kind }

// Features implements servers.Server.
func (s *Server) Features() servers.FeatureSet { return s.features }

// Mount attaches an http.Handler under (method, path). Any HTTP method is
// accepted at the mux level; we filter inside a wrapper.
func (s *Server) Mount(method, path string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := strings.ToUpper(method)
	s.mux.Handle(path, methodFilter(m, h))
}

func methodFilter(method string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) registerStatic() {
	s.mux.Handle("/", methodFilter(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			// /users/:id lives at /users/, so only respond here for exact root.
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Hello, World!"))
	})))
	s.mux.Handle("/json", methodFilter(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.jsonSmall)
	})))
	s.mux.Handle("/json-1k", methodFilter(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.json1K)
	})))
	s.mux.Handle("/json-64k", methodFilter(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.json64K)
	})))
	s.mux.Handle("/users/", methodFilter(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/users/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "User ID: %s", id)
	})))
	s.mux.Handle("/upload", methodFilter(http.MethodPost, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = drainBody(r)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("OK"))
	})))
}

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func drainBody(r *http.Request) (int64, error) {
	if r.Body == nil {
		return 0, nil
	}
	defer r.Body.Close()
	var n int64
	buf := make([]byte, 32*1024)
	for {
		m, err := r.Body.Read(buf)
		n += int64(m)
		if err != nil {
			if err.Error() == "EOF" {
				return n, nil
			}
			return n, nil
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

	var handler http.Handler = s.mux
	h2s := &http2.Server{
		MaxReadFrameSize:     1 << 20,
		MaxConcurrentStreams: 250,
		IdleTimeout:          120 * time.Second,
	}
	switch s.mode {
	case modeH1:
		// leave handler as plain mux; don't configure h2 on server.
	case modeH2C:
		// h2c.NewHandler happily upgrades h2c-upgrade and accepts
		// prior-knowledge preface too.
		handler = h2c.NewHandler(s.mux, h2s)
	case modeAuto:
		handler = h2c.NewHandler(s.mux, h2s)
	}

	s.srv = &http.Server{
		Handler:      handler,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}
	// For Auto and H2C, also wire http2 on the server so prior-knowledge
	// clients work without hitting h2c upgrade paths.
	if s.mode == modeAuto || s.mode == modeH2C {
		_ = http2.ConfigureServer(s.srv, h2s)
	}
	go func() {
		_ = s.srv.Serve(ln)
	}()
	return ln, nil
}

// Stop implements servers.Server.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// payloadSmall mirrors the /json shape across every perfmatrix server.
type payloadSmall struct {
	Message string `json:"message"`
	Server  string `json:"server"`
}

// buildJSONPayload mirrors the celeris builder: ~targetBytes JSON of shape
// {"size":N,"data":"aaa..."}.
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
		panic(fmt.Sprintf("stdhttp perfmatrix: build payload failed: %v", err))
	}
	return out
}

func init() {
	servers.Register(newServer("stdhttp-h1", modeH1))
	servers.Register(newServer("stdhttp-h2c", modeH2C))
	servers.Register(newServer("stdhttp-auto", modeAuto))
}
