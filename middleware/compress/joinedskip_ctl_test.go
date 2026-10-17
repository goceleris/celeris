package compress

import (
	"os"
	"testing"
)

// CITALLY negative control (throwaway): writes to stdout without a newline and skips, so go test -v
// prints "x--- SKIP: TestJoinedSkipControl (0.00s)", the line the old tally did not count.
func TestJoinedSkipControl(t *testing.T) {
	_, _ = os.Stdout.WriteString("x")
	t.Skip("planted skip")
}
