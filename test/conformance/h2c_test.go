package conformance

import (
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

func TestH2CLifecycle(t *testing.T) {
	for _, ef := range Factories() {
		if !ef.Available() {
			continue
		}
		t.Run(ef.Name, func(t *testing.T) {
			t.Parallel()
			addr, cleanup := startEngine(t, ef, engine.H2C)
			defer cleanup()

			transport := &http.Transport{Protocols: h2cOnly()}

			client := &http.Client{
				Transport: transport,
				Timeout:   5 * time.Second,
			}

			resp, err := client.Get("http://" + addr + "/h2c")
			if err != nil {
				t.Fatalf("h2c GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 200 {
				t.Errorf("h2c status: got %d, want 200", resp.StatusCode)
			}
			if resp.ProtoMajor != 2 {
				t.Errorf("expected HTTP/2, got HTTP/%d.%d", resp.ProtoMajor, resp.ProtoMinor)
			}
		})
	}
}
