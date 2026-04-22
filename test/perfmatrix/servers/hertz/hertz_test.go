package hertz

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
	wants := []string{"hertz-h1", "hertz-h2c", "hertz-auto"}
	have := map[string]bool{}
	for _, s := range servers.Registry() {
		if s.Kind() == "hertz" {
			have[s.Name()] = true
		}
	}
	for _, n := range wants {
		if !have[n] {
			t.Errorf("%s not registered", n)
		}
	}
}

// TestSmokeH1 only covers the H1 mode; H2C requires a prior-knowledge
// HTTP/2 client which stdlib http.Client doesn't do without TLS. The
// integration matrix test covers H2C via loadgen's own client.
func TestSmokeH1(t *testing.T) {
	s := newServer("hertz-smoke-h1", modeH1)
	smoke(t, s)
}

func TestSmokeAutoViaH1(t *testing.T) {
	// Auto mode accepts H1 too, so we can smoke it via plain http.Client.
	s := newServer("hertz-smoke-auto", modeAuto)
	smoke(t, s)
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
	// hertz needs a moment for its transport to finish setup.
	time.Sleep(250 * time.Millisecond)

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
