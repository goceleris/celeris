package resource

import (
	"math"
	"testing"
	"time"
)

// A timeout of math.MaxInt64 is kept as a hundred years, for every timeout, so that
// `now + timeout` in the engines (detectDeadline, the h2 deadlines) cannot overflow.
func TestHugeTimeoutsAreClamped889(t *testing.T) {
	never := time.Duration(math.MaxInt64)
	c := Config{ReadTimeout: never, ReadHeaderTimeout: never, WriteTimeout: never, IdleTimeout: never}.WithDefaults()
	for name, got := range map[string]time.Duration{"read": c.ReadTimeout, "readHeader": c.ReadHeaderTimeout, "write": c.WriteTimeout, "idle": c.IdleTimeout} {
		if got != MaxTimeout {
			t.Errorf("%s timeout = %v, want %v", name, got, MaxTimeout)
		}
		if now := time.Now().UnixNano(); now+int64(got) < now {
			t.Errorf("%s: now + timeout overflows", name)
		}
	}
	// And the second pass (the engine constructor's) keeps it, and an ordinary value stays N.
	c2 := Config{ReadTimeout: 5 * time.Second}.WithDefaults().WithDefaults()
	if c2.ReadTimeout != 5*time.Second || c.WithDefaults().WriteTimeout != MaxTimeout {
		t.Errorf("second pass: %v %v", c2.ReadTimeout, c.WithDefaults().WriteTimeout)
	}
}
