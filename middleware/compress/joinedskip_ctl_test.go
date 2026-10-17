package compress

import (
	"os"
	"testing"
)

// CITALLY negative control (throwaway): a deferred write to stdout without a newline runs after the
// skip is logged and before go test prints the result line, so -v prints
// "x--- SKIP: TestJoinedSkipControl (0.00s)", the line the old tally did not count.
func TestJoinedSkipControl(t *testing.T) {
	defer func() { _, _ = os.Stdout.WriteString("x") }()
	t.Skip("planted skip")
}
