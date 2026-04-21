// Pipelined HTTP/1.1 throughput comparison.
// A single TCP connection sends pipelineBatch back-to-back requests
// without waiting for individual responses, then drains all responses
// in order. Tests the real production pattern where CDNs, proxies, and
// high-throughput API clients pipeline H1 requests to amortize TCP
// round-trips — loadgen's keep-alive pattern is request/response
// lock-step, so this fills a gap.
package benchcmp

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
	"github.com/go-chi/chi/v5"
	"github.com/gofiber/fiber/v3"
	"github.com/labstack/echo/v4"
	"github.com/valyala/fasthttp"
)

const pipelineBatch = 16

func pipelineRun(b *testing.B, addr, req string) {
	b.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	_ = conn.(*net.TCPConn).SetNoDelay(true)
	reader := bufio.NewReaderSize(conn, 64*1024)

	batch := make([]byte, 0, len(req)*pipelineBatch)
	for i := 0; i < pipelineBatch; i++ {
		batch = append(batch, req...)
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(pipelineBatch))

	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(batch); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < pipelineBatch; j++ {
			status, err := reader.ReadSlice('\n')
			if err != nil {
				b.Fatal(err)
			}
			if len(status) < 12 || status[9] != '2' {
				b.Fatalf("bad status: %q", status)
			}
			contentLen := -1
			for {
				line, err := reader.ReadSlice('\n')
				if err != nil {
					b.Fatal(err)
				}
				if len(line) <= 2 {
					break
				}
				if len(line) > 16 && (line[0] == 'C' || line[0] == 'c') {
					if cl, ok := pipelineParseCL(line); ok {
						contentLen = cl
					}
				}
			}
			if contentLen > 0 {
				if _, err := reader.Discard(contentLen); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
}

func pipelineParseCL(line []byte) (int, bool) {
	if len(line) < 17 {
		return -1, false
	}
	if !(line[0] == 'C' || line[0] == 'c') {
		return -1, false
	}
	i := 0
	for i < len(line) && line[i] != ':' {
		i++
	}
	if i == len(line) {
		return -1, false
	}
	i++
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	n := 0
	any := false
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		n = n*10 + int(line[i]-'0')
		i++
		any = true
	}
	if !any {
		return -1, false
	}
	return n, true
}

const pipelineReq = "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"

func BenchmarkPipelineH1_Celeris(b *testing.B) {
	s := celeris.New(celeris.Config{Engine: celeris.IOUring})
	s.GET("/", func(c *celeris.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go func() { _ = s.StartWithListener(ln) }()
	defer func() { _ = s.Shutdown(b.Context()) }()
	deadline := time.Now().Add(3 * time.Second)
	for s.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if s.Addr() == nil {
		b.Fatal("server did not bind")
	}
	pipelineRun(b, s.Addr().String(), pipelineReq)
}

func BenchmarkPipelineH1_Fiber(b *testing.B) {
	app := fiber.New(fiber.Config{})
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	server := &fasthttp.Server{Handler: app.Handler()}
	go func() { _ = server.Serve(ln) }()
	defer func() { _ = server.Shutdown() }()
	time.Sleep(50 * time.Millisecond)
	pipelineRun(b, ln.Addr().String(), pipelineReq)
}

func BenchmarkPipelineH1_Chi(b *testing.B) {
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Hello, World!"))
	})
	ts := httptest.NewServer(r)
	defer ts.Close()
	pipelineRun(b, ts.Listener.Addr().String(), pipelineReq)
}

func BenchmarkPipelineH1_Echo(b *testing.B) {
	e := echo.New()
	e.HideBanner = true
	e.Logger.SetOutput(pipelineNopWriter{})
	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	e.Listener = ln
	go func() { _ = e.Start("") }()
	defer func() { _ = e.Close() }()
	time.Sleep(50 * time.Millisecond)
	pipelineRun(b, ln.Addr().String(), pipelineReq)
}

func BenchmarkPipelineH1_Stdlib(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Hello, World!"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	pipelineRun(b, ts.Listener.Addr().String(), pipelineReq)
}

type pipelineNopWriter struct{}

func (pipelineNopWriter) Write(p []byte) (int, error) { return len(p), nil }
