package iris

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
	wants := []string{"iris-h1", "iris-h2c", "iris-auto"}
	have := map[string]bool{}
	for _, s := range servers.Registry() {
		if s.Kind() == "iris" {
			have[s.Name()] = true
		}
	}
	for _, n := range wants {
		if !have[n] {
			t.Errorf("%s not registered", n)
		}
	}
}

func TestSmokeAll(t *testing.T) {
	modes := []mode{modeH1, modeH2C, modeAuto}
	names := []string{"h1", "h2c", "auto"}
	for i, m := range modes {
		s := newServer("iris-smoke-"+names[i], m)
		smoke(t, s)
	}
}

func smoke(t *testing.T, s *Server) {
	t.Helper()
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
	for _, path := range []string{"/", "/json", "/users/42"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("%s GET %s: %v", s.Name(), path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || len(b) == 0 {
			t.Fatalf("%s GET %s status=%d body=%q", s.Name(), path, resp.StatusCode, b)
		}
	}
	resp, err := client.Post(base+"/upload", "application/octet-stream", strings.NewReader("hi"))
	if err != nil {
		t.Fatalf("%s POST /upload: %v", s.Name(), err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "OK" {
		t.Fatalf("%s POST /upload status=%d body=%q", s.Name(), resp.StatusCode, b)
	}
}
