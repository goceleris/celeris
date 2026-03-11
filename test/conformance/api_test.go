package conformance

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

func startAPIServer(t *testing.T, cfg celeris.Config) (addr string, cleanup func()) {
	t.Helper()

	port := freePort(t)
	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)

	s := celeris.New(cfg)
	s.GET("/hello", func(c *celeris.Context) error {
		return c.String(200, "hello")
	})
	s.POST("/echo", func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", c.Body())
	})
	s.GET("/users/:id", func(c *celeris.Context) error {
		return c.String(200, "user-%s", c.Param("id"))
	})
	s.GET("/json", func(c *celeris.Context) error {
		return c.JSON(200, map[string]string{"status": "ok"})
	})
	s.DELETE("/items/:id", func(c *celeris.Context) error {
		return c.NoContent(204)
	})
	s.GET("/query", func(c *celeris.Context) error {
		return c.String(200, "q=%s", c.Query("q"))
	})
	s.GET("/header", func(c *celeris.Context) error {
		return c.String(200, "x=%s", c.Header("x-test"))
	})

	api := s.Group("/api")
	api.GET("/items", func(c *celeris.Context) error {
		return c.String(200, "items")
	})
	api.GET("/items/:id", func(c *celeris.Context) error {
		return c.String(200, "item-%s", c.Param("id"))
	})

	ctx := t.Context()
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.StartWithContext(ctx)
	}()

	addr = fmt.Sprintf("127.0.0.1:%d", port)

	// Wait for the server to be ready.
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/hello")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case err := <-errCh:
			if err != nil {
				t.Skipf("server failed to start: %v", err)
			}
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}

	return addr, func() {}
}

func TestAPIRouting(t *testing.T) {
	addr, cleanup := startAPIServer(t, celeris.Config{Engine: celeris.Std})
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("GET /hello", func(t *testing.T) {
		resp, err := client.Get("http://" + addr + "/hello")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "hello" {
			t.Fatalf("expected 200 hello, got %d %s", resp.StatusCode, string(body))
		}
	})

	t.Run("POST /echo", func(t *testing.T) {
		resp, err := client.Post("http://"+addr+"/echo", "text/plain", strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "payload" {
			t.Fatalf("expected 200 payload, got %d %s", resp.StatusCode, string(body))
		}
	})

	t.Run("GET /users/:id", func(t *testing.T) {
		resp, err := client.Get("http://" + addr + "/users/42")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "user-42" {
			t.Fatalf("expected 200 user-42, got %d %s", resp.StatusCode, string(body))
		}
	})

	t.Run("GET /json", func(t *testing.T) {
		resp, err := client.Get("http://" + addr + "/json")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(body), `"status":"ok"`) {
			t.Fatalf("expected 200 json, got %d %s", resp.StatusCode, string(body))
		}
	})

	t.Run("DELETE /items/:id", func(t *testing.T) {
		req, _ := http.NewRequest("DELETE", "http://"+addr+"/items/99", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 204 {
			t.Fatalf("expected 204, got %d", resp.StatusCode)
		}
	})

	t.Run("GET /query", func(t *testing.T) {
		resp, err := client.Get("http://" + addr + "/query?q=test")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "q=test" {
			t.Fatalf("expected q=test, got %s", string(body))
		}
	})

	t.Run("GET /header", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://"+addr+"/header", nil)
		req.Header.Set("X-Test", "hello")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "x=hello" {
			t.Fatalf("expected x=hello, got %s", string(body))
		}
	})

	t.Run("GET /api/items", func(t *testing.T) {
		resp, err := client.Get("http://" + addr + "/api/items")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "items" {
			t.Fatalf("expected 200 items, got %d %s", resp.StatusCode, string(body))
		}
	})

	t.Run("GET /api/items/:id", func(t *testing.T) {
		resp, err := client.Get("http://" + addr + "/api/items/7")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "item-7" {
			t.Fatalf("expected 200 item-7, got %d %s", resp.StatusCode, string(body))
		}
	})

	t.Run("404 Not Found", func(t *testing.T) {
		resp, err := client.Get("http://" + addr + "/nonexistent")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 404 {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})
}
