package stdhttp

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
	wants := map[string]servers.FeatureSet{
		"stdhttp-h1":   {HTTP1: true, Drivers: true, Middleware: true},
		"stdhttp-h2c":  {HTTP2C: true, H2CUpgrade: true, Drivers: true, Middleware: true},
		"stdhttp-auto": {HTTP1: true, HTTP2C: true, Auto: true, H2CUpgrade: true, Drivers: true, Middleware: true},
	}
	found := map[string]bool{}
	for _, s := range servers.Registry() {
		if s.Kind() != "stdhttp" {
			continue
		}
		want, ok := wants[s.Name()]
		if !ok {
			t.Errorf("unexpected stdhttp server name: %q", s.Name())
			continue
		}
		found[s.Name()] = true
		if s.Features() != want {
			t.Errorf("%s Features = %+v, want %+v", s.Name(), s.Features(), want)
		}
	}
	for n := range wants {
		if !found[n] {
			t.Errorf("%s not registered", n)
		}
	}
}

func TestH1Smoke(t *testing.T) {
	s := newServer("stdhttp-h1-smoke", modeH1)
	smokeHTTP1(t, s)
}

func TestH2CSmoke(t *testing.T) {
	s := newServer("stdhttp-h2c-smoke", modeH2C)
	// h2c also accepts classical H1 requests via its handler wrapper, so a
	// plain http.Client is enough for the smoke check.
	smokeHTTP1(t, s)
}

func TestAutoSmoke(t *testing.T) {
	s := newServer("stdhttp-auto-smoke", modeAuto)
	smokeHTTP1(t, s)
}

func smokeHTTP1(t *testing.T, s *Server) {
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
