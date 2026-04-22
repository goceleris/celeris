package benchcmp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofiber/fiber/v3"
	"github.com/labstack/echo/v4"
	"github.com/valyala/fasthttp"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// SSE throughput: long-lived connection, server pushes events as fast as
// possible, client reads one event per bench iteration. Measures ns/event
// and reports events/sec via b.ReportMetric.
//
// Each framework emits the same event shape:
//
//	data: msg-NNNN\n\n
//
// which is the cheapest valid SSE event and isolates framework overhead
// rather than event-payload cost.

const sseMsgPrefix = "data: msg-"

// readOneEvent consumes lines from r until a blank line terminator (the end
// of a single SSE event). Returns the payload byte count (for b.SetBytes).
func readOneEvent(br *bufio.Reader) (int, error) {
	total := 0
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return total, err
		}
		total += len(line)
		if len(line) <= 2 { // "\n" or "\r\n"
			return total, nil
		}
	}
}

// benchmarkSSE runs b.N read iterations against a server that streams
// events until the client disconnects. Each framework wires a handler that
// loops `for { write one event; flush }`.
func benchmarkSSE(b *testing.B, start func() (url string, stop func())) {
	url, stop := start()
	defer stop()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		b.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	// Transport that bypasses HTTP/2 and default keepalive shenanigans so
	// every framework is compared on the same plain H1 path.
	tr := &http.Transport{
		DisableKeepAlives:  false,
		ForceAttemptHTTP2:  false,
		MaxIdleConnsPerHost: 1,
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("status=%d", resp.StatusCode)
	}
	br := bufio.NewReaderSize(resp.Body, 32<<10)

	b.ReportAllocs()
	b.ResetTimer()
	var bytes int64
	for i := 0; i < b.N; i++ {
		n, err := readOneEvent(br)
		if err != nil {
			b.Fatalf("read %d: %v", i, err)
		}
		bytes += int64(n)
	}
	b.SetBytes(bytes / int64(b.N))
}

// --- Celeris --------------------------------------------------------

func startSSECeleris() (string, func()) {
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0"})
	srv.GET("/events", sse.New(sse.Config{
		HeartbeatInterval: 0,
		Handler: func(c *sse.Client) {
			i := 0
			for {
				if err := c.SendData("msg-" + strconv.Itoa(i)); err != nil {
					return
				}
				i++
			}
		},
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() { _ = srv.StartWithListener(ln) }()
	return "http://" + ln.Addr().String() + "/events", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func BenchmarkSSE_Celeris(b *testing.B) {
	benchmarkSSE(b, startSSECeleris)
}

// --- stdhttp -------------------------------------------------------

func startSSEStdhttp() (string, func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		ctx := r.Context()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if _, err := fmt.Fprintf(w, "data: msg-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	return srv.URL + "/events", srv.Close
}

func BenchmarkSSE_Stdhttp(b *testing.B) {
	benchmarkSSE(b, startSSEStdhttp)
}

// --- chi -----------------------------------------------------------

func startSSEChi() (string, func()) {
	r := chi.NewRouter()
	r.Get("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		ctx := r.Context()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if _, err := fmt.Fprintf(w, "data: msg-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
		}
	})
	srv := httptest.NewServer(r)
	return srv.URL + "/events", srv.Close
}

func BenchmarkSSE_Chi(b *testing.B) {
	benchmarkSSE(b, startSSEChi)
}

// --- echo ----------------------------------------------------------

func startSSEEcho() (string, func()) {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/events", func(c echo.Context) error {
		c.Response().Header().Set("Content-Type", "text/event-stream")
		c.Response().Header().Set("Cache-Control", "no-cache")
		c.Response().WriteHeader(http.StatusOK)
		ctx := c.Request().Context()
		w := c.Response()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			if _, err := fmt.Fprintf(w, "data: msg-%d\n\n", i); err != nil {
				return nil
			}
			w.Flush()
		}
	})
	srv := httptest.NewServer(e)
	return srv.URL + "/events", srv.Close
}

func BenchmarkSSE_Echo(b *testing.B) {
	benchmarkSSE(b, startSSEEcho)
}

// --- fiber (v3) ----------------------------------------------------

func startSSEFiber() (string, func()) {
	app := fiber.New()
	app.Get("/events", func(c fiber.Ctx) error {
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")
		return c.SendStreamWriter(func(w *bufio.Writer) {
			for i := 0; ; i++ {
				if _, err := fmt.Fprintf(w, "data: msg-%d\n\n", i); err != nil {
					return
				}
				if err := w.Flush(); err != nil {
					return
				}
			}
		})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	var closed atomic.Bool
	go func() {
		_ = fasthttp.Serve(ln, app.Handler())
		closed.Store(true)
	}()
	return "http://" + ln.Addr().String() + "/events", func() {
		_ = ln.Close()
		_ = app.Shutdown()
	}
}

func BenchmarkSSE_Fiber(b *testing.B) {
	benchmarkSSE(b, startSSEFiber)
}

// Ensure io.Discard is referenced from this file so gofmt doesn't touch
// the import block if we need it later for variants (e.g. multi-line events).
var _ = io.Discard
