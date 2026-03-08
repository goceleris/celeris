package conformance

import (
	"testing"

	"github.com/goceleris/celeris/engine"
)

func TestAutoProtocolDetection(t *testing.T) {
	for _, ef := range Factories() {
		if !ef.Available() {
			continue
		}
		t.Run(ef.Name+"/AutoHTTP1", func(t *testing.T) {
			t.Parallel()
			addr, cleanup := startEngine(t, ef, engine.Auto)
			defer cleanup()

			resp := sendRequest(t, addr, "GET", "/auto", nil)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 200 {
				t.Errorf("auto detection HTTP/1: status %d", resp.StatusCode)
			}
		})
	}
}
