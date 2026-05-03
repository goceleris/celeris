package fasthttp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// TestRegistered verifies fasthttp-h1 shows up in the global registry
// with the documented name convention.
func TestRegistered(t *testing.T) {
	var found servers.Server
	for _, s := range servers.Registry() {
		if s.Name() == "fasthttp-h1" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatal("fasthttp-h1 not in registry")
	}
	if found.Kind() != "fasthttp" {
		t.Errorf("Kind = %q, want fasthttp", found.Kind())
	}
	f := found.Features()
	if !f.HTTP1 || f.HTTP2C || f.Auto {
		t.Errorf("Features = %+v, want H1-only", f)
	}
}

// TestStartSmoke boots the server and hits /json, /, /users/:id, /upload.
func TestStartSmoke(t *testing.T) {
	s := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := s.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = s.Stop(stopCtx)
	})

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(base + "/json")
	if err != nil {
		t.Fatalf("GET /json: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("GET /json status=%d body=%q", resp.StatusCode, body)
	}

	resp, err = client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "Hello, World!" {
		t.Fatalf("GET / status=%d body=%q", resp.StatusCode, body)
	}

	resp, err = client.Get(base + "/users/42")
	if err != nil {
		t.Fatalf("GET /users/42: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "User ID: 42" {
		t.Fatalf("GET /users/42 status=%d body=%q", resp.StatusCode, body)
	}

	resp, err = client.Post(base+"/upload", "application/octet-stream", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "OK" {
		t.Fatalf("POST /upload status=%d body=%q", resp.StatusCode, body)
	}
}
