package spec

import (
	"net"
	"os/exec"
	"strconv"
	"testing"

	"github.com/goceleris/celeris/engine"
)

// TestH2Spec runs the h2spec conformance suite against every available engine in H2C mode.
// Install h2spec with: mage tools
func TestH2Spec(t *testing.T) {
	h2specBin, err := exec.LookPath("h2spec")
	if err != nil {
		t.Skip("h2spec not found in PATH; install with: mage tools")
	}

	for _, se := range specEngines {
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngine(t, se, engine.H2C)

			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("parse addr %s: %v", addr, err)
			}
			port, _ := strconv.Atoi(portStr)

			sections := []string{"generic", "http2", "hpack"}
			for _, section := range sections {
				t.Run(section, func(t *testing.T) {
					cmd := exec.Command(h2specBin,
						"-h", host,
						"-p", strconv.Itoa(port),
						"-o", "5",
						section,
					)
					output, err := cmd.CombinedOutput()
					checkH2SpecResult(t, se.name, section, output, err)
				})
			}
		})
	}
}
