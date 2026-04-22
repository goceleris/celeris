// Package fiber registers the fiber v3 H1 server against the
// perfmatrix registry. fiber does not support HTTP/2, so only the "h1"
// cell-column is registered.
package fiber

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	fiberv3 "github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp/fasthttpadaptor"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

const kind = "fiber"

type payloadSmall struct {
	Message string `json:"message"`
	Server  string `json:"server"`
}

// Server is the fiber v3 H1 implementation.
type Server struct {
	name     string
	features servers.FeatureSet

	jsonSmall []byte
	json1K    []byte
	json64K   []byte

	mu     sync.Mutex
	app    *fiberv3.App
	ln     net.Listener
	errCh  chan error
	closed chan struct{}

	drivers       *driverState
	mountedChain  bool
	mountedDriver bool
}

// New returns the fiber-h1 Server.
func New() *Server {
	s := &Server{
		name: "fiber-h1",
		features: servers.FeatureSet{
			HTTP1:      true,
			Drivers:    true,
			Middleware: true,
		},
	}
	small, _ := json.Marshal(payloadSmall{Message: "Hello, World!", Server: kind})
	s.jsonSmall = small
	s.json1K = buildJSONPayload(1024)
	s.json64K = buildJSONPayload(64 * 1024)
	s.ensureApp()
	return s
}

// Name implements servers.Server.
func (s *Server) Name() string { return s.name }

// Kind implements servers.Server.
func (s *Server) Kind() string { return kind }

// Features implements servers.Server.
func (s *Server) Features() servers.FeatureSet { return s.features }

// Mount attaches a net/http.Handler under (method, path). Wave-3 agents
// call this before Start. Prefer MountNative on hot paths to avoid the
// fasthttpadaptor hop.
func (s *Server) Mount(method, path string, h http.Handler) {
	s.ensureApp()
	native := func(c fiberv3.Ctx) error {
		fasthttpadaptor.NewFastHTTPHandler(h)(c.RequestCtx())
		return nil
	}
	s.MountNative(method, path, native)
}

// MountNative attaches a native fiber handler under (method, path).
func (s *Server) MountNative(method, path string, h fiberv3.Handler) {
	s.ensureApp()
	s.app.Add([]string{strings.ToUpper(method)}, path, h)
}

func (s *Server) ensureApp() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.app != nil {
		return
	}
	s.app = fiberv3.New(fiberv3.Config{
		ServerHeader:  "perfmatrix-fiber",
		StrictRouting: true,
		BodyLimit:     100 << 20,
	})
	s.registerStatic()
}

func (s *Server) registerStatic() {
	s.app.Get("/", func(c fiberv3.Ctx) error {
		c.Set("Content-Type", "text/plain; charset=utf-8")
		return c.SendString("Hello, World!")
	})
	s.app.Get("/json", func(c fiberv3.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send(s.jsonSmall)
	})
	s.app.Get("/json-1k", func(c fiberv3.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send(s.json1K)
	})
	s.app.Get("/json-64k", func(c fiberv3.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send(s.json64K)
	})
	s.app.Get("/users/:id", func(c fiberv3.Ctx) error {
		c.Set("Content-Type", "text/plain; charset=utf-8")
		return c.SendString(fmt.Sprintf("User ID: %s", c.Params("id")))
	})
	s.app.Post("/upload", func(c fiberv3.Ctx) error {
		_ = c.Body()
		c.Set("Content-Type", "text/plain; charset=utf-8")
		return c.SendString("OK")
	})
}

// Start implements servers.Server.
func (s *Server) Start(ctx context.Context, svcs *services.Handles) (net.Listener, error) {
	_ = ctx
	s.ensureApp()
	// Wave-3: mount middleware chain routes before driver routes so
	// fiber's per-prefix Use() attaches before the terminal handlers.
	mountChainHandlers(s)
	mountDriverHandlers(s, svcs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.ln = ln
	s.errCh = make(chan error, 1)
	s.closed = make(chan struct{})
	go func() {
		defer close(s.closed)
		s.errCh <- s.app.Listener(ln, fiberv3.ListenConfig{DisableStartupMessage: true})
	}()
	return ln, nil
}

// Stop implements servers.Server.
func (s *Server) Stop(ctx context.Context) error {
	if s.app == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.app.ShutdownWithContext(ctx) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	s.shutdownDriverHandlers()
	return err
}

// buildJSONPayload mirrors the celeris package's builder: ~targetBytes
// JSON doc of shape {"size":N,"data":"aaa...aaa"}.
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
		panic(fmt.Sprintf("fiber perfmatrix: build payload failed: %v", err))
	}
	return out
}

func init() { servers.Register(New()) }
