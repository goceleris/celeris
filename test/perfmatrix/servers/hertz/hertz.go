// Package hertz registers cloudwego/hertz against the perfmatrix
// registry. Three flavours: H1, H2C (via hertz-contrib/http2), Auto.
package hertz

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	h2config "github.com/hertz-contrib/http2/config"
	h2factory "github.com/hertz-contrib/http2/factory"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

const kind = "hertz"

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

// Server is the hertz implementation.
type Server struct {
	name     string
	mode     mode
	features servers.FeatureSet

	jsonSmall []byte
	json1K    []byte
	json64K   []byte

	mu  sync.Mutex
	h   *server.Hertz
	ln  net.Listener
	run chan error

	drivers       *driverState
	mountedChain  bool
	mountedDriver bool
}

func newServer(name string, m mode) *Server {
	s := &Server{name: name, mode: m}
	switch m {
	case modeH1:
		s.features = servers.FeatureSet{HTTP1: true, Drivers: true, Middleware: true}
	case modeH2C:
		// hertz-contrib/http2 speaks prior-knowledge H2C; it does not
		// advertise Upgrade: h2c. H2CUpgrade stays false so loadgen
		// dials in prior-knowledge mode.
		s.features = servers.FeatureSet{HTTP2C: true, Drivers: true, Middleware: true}
	case modeAuto:
		s.features = servers.FeatureSet{HTTP1: true, HTTP2C: true, Auto: true, Drivers: true, Middleware: true}
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

// Mount attaches an http.Handler under (method, path). Prefer
// MountNative for hot paths.
func (s *Server) Mount(method, path string, h http.Handler) {
	s.MountNative(method, path, httpHandlerToHertz(h))
}

// MountNative attaches a hertz-native handler under (method, path).
func (s *Server) MountNative(method, path string, h app.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureEngineLocked(nil)
	s.h.Handle(strings.ToUpper(method), path, h)
}

// Start implements servers.Server.
func (s *Server) Start(ctx context.Context, svcs *services.Handles) (net.Listener, error) {
	_ = ctx
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.ln = ln

	s.mu.Lock()
	s.ensureEngineLocked(ln)
	s.mu.Unlock()

	// Mount chain + driver routes after the engine is built (hertz
	// accepts route additions any time before Run; both mount funcs
	// take their own locks).
	mountChainHandlers(s)
	mountDriverHandlers(s, svcs)

	s.run = make(chan error, 1)
	go func() { s.run <- s.h.Run() }()

	// Wait briefly for hertz to bind; Run returns when the engine stops,
	// so we poll IsRunning.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.h.IsRunning() {
			return ln, nil
		}
		select {
		case err := <-s.run:
			if err != nil {
				return nil, fmt.Errorf("hertz Run: %w", err)
			}
			return ln, nil
		case <-time.After(20 * time.Millisecond):
		}
	}
	return ln, nil
}

// Stop implements servers.Server.
func (s *Server) Stop(ctx context.Context) error {
	if s.h == nil {
		return nil
	}
	err := s.h.Shutdown(ctx)
	s.shutdownDriverHandlers()
	return err
}

// ensureEngineLocked builds the hertz engine once. Called under s.mu.
func (s *Server) ensureEngineLocked(ln net.Listener) {
	if s.h != nil {
		// If we already have an engine but are being asked again with a
		// listener (Start path), the engine is already wired — nothing to
		// do.
		return
	}
	opts := []config.Option{
		server.WithExitWaitTime(50 * time.Millisecond),
		server.WithMaxRequestBodySize(100 << 20),
		server.WithDisablePrintRoute(true),
	}
	if ln != nil {
		opts = append(opts, server.WithListener(ln))
	}
	if s.mode == modeH2C || s.mode == modeAuto {
		// Enable Auto-H2C detection on the same listener. WithH2C(true)
		// tells hertz to serve prior-knowledge h2c alongside H1.
		opts = append(opts, server.WithH2C(true))
	}
	h := server.New(opts...)
	if s.mode == modeH2C || s.mode == modeAuto {
		h2sf := h2factory.NewServerFactory(
			h2config.WithReadTimeout(0),
			h2config.WithMaxReadFrameSize(1<<20),
		)
		h.AddProtocol("h2", h2sf)
	}
	s.h = h
	s.registerStaticLocked()
}

func (s *Server) registerStaticLocked() {
	s.h.GET("/", func(_ context.Context, ctx *app.RequestContext) {
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.SetStatusCode(http.StatusOK)
		_, _ = ctx.Write([]byte("Hello, World!"))
	})
	s.h.GET("/json", func(_ context.Context, ctx *app.RequestContext) {
		writeJSONHertz(ctx, s.jsonSmall)
	})
	s.h.GET("/json-1k", func(_ context.Context, ctx *app.RequestContext) {
		writeJSONHertz(ctx, s.json1K)
	})
	s.h.GET("/json-64k", func(_ context.Context, ctx *app.RequestContext) {
		writeJSONHertz(ctx, s.json64K)
	})
	s.h.GET("/users/:id", func(_ context.Context, ctx *app.RequestContext) {
		id := ctx.Param("id")
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.SetStatusCode(http.StatusOK)
		_, _ = fmt.Fprintf(ctx, "User ID: %s", id)
	})
	s.h.POST("/upload", func(_ context.Context, ctx *app.RequestContext) {
		_ = ctx.Request.Body()
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.SetStatusCode(http.StatusOK)
		_, _ = ctx.Write([]byte("OK"))
	})
}

func writeJSONHertz(ctx *app.RequestContext, body []byte) {
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(http.StatusOK)
	_, _ = ctx.Write(body)
}

// httpHandlerToHertz adapts a net/http.Handler to hertz's signature. This
// is a best-effort bridge; prefer MountNative on hot paths.
func httpHandlerToHertz(h http.Handler) app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		w := &hertzResponseWriter{ctx: ctx, header: http.Header{}}
		req, err := http.NewRequestWithContext(c, string(ctx.Method()), string(ctx.Request.URI().FullURI()), nil)
		if err != nil {
			ctx.SetStatusCode(http.StatusInternalServerError)
			return
		}
		if body := ctx.Request.Body(); len(body) > 0 {
			req.Body = newBodyReader(body)
			req.ContentLength = int64(len(body))
		}
		h.ServeHTTP(w, req)
		w.flush()
	}
}

type hertzResponseWriter struct {
	ctx         *app.RequestContext
	header      http.Header
	status      int
	wroteHeader bool
	buf         []byte
}

func (w *hertzResponseWriter) Header() http.Header { return w.header }

func (w *hertzResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *hertzResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *hertzResponseWriter) flush() {
	if !w.wroteHeader {
		w.status = http.StatusOK
	}
	for k, vs := range w.header {
		for _, v := range vs {
			w.ctx.Response.Header.Add(k, v)
		}
	}
	w.ctx.SetStatusCode(w.status)
	if len(w.buf) > 0 {
		_, _ = w.ctx.Write(w.buf)
	}
}

type bodyReader struct {
	b   []byte
	pos int
}

func newBodyReader(b []byte) *bodyReader { return &bodyReader{b: b} }

func (r *bodyReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, errBodyEOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

func (r *bodyReader) Close() error { return nil }

var errBodyEOF = &eofError{}

type eofError struct{}

func (*eofError) Error() string { return "EOF" }

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
		panic(fmt.Sprintf("hertz perfmatrix: build payload failed: %v", err))
	}
	return out
}

func init() {
	servers.Register(newServer("hertz-h1", modeH1))
	servers.Register(newServer("hertz-h2c", modeH2C))
	servers.Register(newServer("hertz-auto", modeAuto))
}
