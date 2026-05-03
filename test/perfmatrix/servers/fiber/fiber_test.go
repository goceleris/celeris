package fiber

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

func TestRegistered(t *testing.T) {
	var found servers.Server
	for _, s := range servers.Registry() {
		if s.Name() == "fiber-h1" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatal("fiber-h1 not in registry")
	}
	if found.Kind() != "fiber" {
		t.Errorf("Kind = %q, want fiber", found.Kind())
	}
	f := found.Features()
	if !f.HTTP1 || f.HTTP2C || f.Auto {
		t.Errorf("Features = %+v, want H1-only", f)
	}
}

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
	// fiber needs a moment for its listener to be fully wired.
	time.Sleep(150 * time.Millisecond)

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	for _, path := range []string{"/", "/json", "/json-1k", "/json-64k", "/users/42"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("GET %s status=%d body=%q", path, resp.StatusCode, body)
		}
	}
	resp, err := client.Post(base+"/upload", "application/octet-stream", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "OK" {
		t.Fatalf("POST /upload status=%d body=%q", resp.StatusCode, body)
	}
}
