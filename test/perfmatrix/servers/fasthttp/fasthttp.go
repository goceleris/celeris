// Package fasthttp registers the valyala/fasthttp H1 server against
// the perfmatrix registry. fasthttp is H1-only.
package fasthttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

const kind = "fasthttp"

// payloadSmall mirrors the /json shape used by every perfmatrix server.
type payloadSmall struct {
	Message string `json:"message"`
	Server  string `json:"server"`
}

// Server is the fasthttp H1 implementation.
type Server struct {
	name     string
	features servers.FeatureSet

	jsonSmall []byte
	json1K    []byte
	json64K   []byte

	mu     sync.RWMutex
	extras map[string]fasthttp.RequestHandler // "METHOD path" → handler
	srv    *fasthttp.Server
	ln     net.Listener
}

// New returns the fasthttp-h1 Server.
func New() *Server {
	s := &Server{
		name: "fasthttp-h1",
		features: servers.FeatureSet{
			HTTP1:      true,
			Drivers:    true,
			Middleware: true,
		},
		extras: make(map[string]fasthttp.RequestHandler),
	}
	small, _ := json.Marshal(payloadSmall{Message: "Hello, World!", Server: kind})
	s.jsonSmall = small
	s.json1K = buildJSONPayload(1024)
	s.json64K = buildJSONPayload(64 * 1024)
	return s
}

// Name implements servers.Server.
func (s *Server) Name() string { return s.name }

// Kind implements servers.Server.
func (s *Server) Kind() string { return kind }

// Features implements servers.Server.
func (s *Server) Features() servers.FeatureSet { return s.features }

// Mount attaches a net/http.Handler under (method, path). Called by later
// waves before Start; adapted via fasthttpadaptor.
func (s *Server) Mount(method, path string, h http.Handler) {
	s.MountNative(method, path, fasthttpadaptor.NewFastHTTPHandler(h))
}

// MountNative attaches a native fasthttp handler under (method, path).
func (s *Server) MountNative(method, path string, h fasthttp.RequestHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extras[strings.ToUpper(method)+" "+path] = h
}

// Start implements servers.Server.
func (s *Server) Start(ctx context.Context, _ *services.Handles) (net.Listener, error) {
	_ = ctx
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.ln = ln
	s.srv = &fasthttp.Server{
		Handler:               s.dispatch,
		Name:                  "perfmatrix-fasthttp",
		NoDefaultServerHeader: true,
		MaxRequestBodySize:    100 << 20,
	}
	go func() { _ = s.srv.Serve(ln) }()
	return ln, nil
}

// Stop implements servers.Server.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.srv.Shutdown() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) dispatch(rc *fasthttp.RequestCtx) {
	method := string(rc.Method())
	path := string(rc.Path())
	switch {
	case method == http.MethodGet && path == "/":
		rc.SetContentType("text/plain; charset=utf-8")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write([]byte("Hello, World!"))
		return
	case method == http.MethodGet && path == "/json":
		writeJSON(rc, s.jsonSmall)
		return
	case method == http.MethodGet && path == "/json-1k":
		writeJSON(rc, s.json1K)
		return
	case method == http.MethodGet && path == "/json-64k":
		writeJSON(rc, s.json64K)
		return
	case method == http.MethodGet && strings.HasPrefix(path, "/users/"):
		id := path[len("/users/"):]
		if id == "" || strings.Contains(id, "/") {
			rc.SetStatusCode(fasthttp.StatusNotFound)
			return
		}
		rc.SetContentType("text/plain; charset=utf-8")
		rc.SetStatusCode(fasthttp.StatusOK)
		fmt.Fprintf(rc, "User ID: %s", id)
		return
	case method == http.MethodPost && path == "/upload":
		_ = rc.PostBody()
		rc.SetContentType("text/plain; charset=utf-8")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write([]byte("OK"))
		return
	}
	s.mu.RLock()
	h, ok := s.extras[method+" "+path]
	s.mu.RUnlock()
	if ok {
		h(rc)
		return
	}
	rc.SetStatusCode(fasthttp.StatusNotFound)
}

func writeJSON(rc *fasthttp.RequestCtx, body []byte) {
	rc.SetContentType("application/json")
	rc.SetStatusCode(fasthttp.StatusOK)
	_, _ = rc.Write(body)
}

// buildJSONPayload mirrors the celeris package's builder: returns a
// ~targetBytes JSON doc of shape {"size":N,"data":"aaa...aaa"}. Pre-
// generated once per Server so /json-1k and /json-64k stream a constant
// byte stream across runs.
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
		panic(fmt.Sprintf("fasthttp perfmatrix: build payload failed: %v", err))
	}
	return out
}

func init() { servers.Register(New()) }
