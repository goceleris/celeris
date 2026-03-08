package spec

import (
	"net"
	"os/exec"
	"strconv"
	"testing"

	"github.com/goceleris/celeris/engine"
)

// TestH2SpecFull runs the complete h2spec suite (all sections, strict mode)
// against each engine sequentially. This covers all 146 test cases.
func TestH2SpecFull(t *testing.T) {
	h2specBin, err := exec.LookPath("h2spec")
	if err != nil {
		t.Skip("h2spec not found in PATH")
	}

	for _, se := range specEngines {
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngine(t, se, engine.H2C)
			host, portStr, _ := net.SplitHostPort(addr)
			port, _ := strconv.Atoi(portStr)
			cmd := exec.Command(h2specBin,
				"-h", host, "-p", strconv.Itoa(port),
				"-o", "10",
				"--strict",
			)
			output, err := cmd.CombinedOutput()
			t.Logf("h2spec:\n%s", output)
			if err != nil {
				t.Errorf("h2spec: %v", err)
			}
		})
	}
}
