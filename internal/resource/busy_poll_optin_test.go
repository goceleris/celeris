package resource

import (
	"math"
	"strings"
	"testing"
	"time"
)

// SO_BUSY_POLL is opt-in (celeris#1036): Resources.SocketBusyPoll
// is 0 unless the caller sets it, and 0 reaches the engines as "off".

func TestSocketBusyPollDefaultsToOff(t *testing.T) {
	if got := (Resources{}).Resolve().SocketBusyPoll; got != 0 {
		t.Errorf("Resources{}.Resolve().SocketBusyPoll = %v, want 0 (busy poll is opt-in)", got)
	}
	// WithDefaults must not turn the unset field into a budget, however many
	// times the engines normalise the Config (Server.doPrepare, the engine
	// constructor, adaptive's sub-engines).
	c := Config{}.WithDefaults().WithDefaults().WithDefaults()
	if got := c.Resources.Resolve().SocketBusyPoll; got != 0 {
		t.Errorf("after WithDefaults x3, SocketBusyPoll resolves to %v, want 0", got)
	}
}

func TestSocketBusyPollResolvesAsSet(t *testing.T) {
	for _, d := range []time.Duration{time.Microsecond, 37 * time.Microsecond, 50 * time.Microsecond, time.Millisecond} {
		r := Resources{SocketBusyPoll: d}
		if got := r.Resolve().SocketBusyPoll; got != d {
			t.Errorf("Resolve().SocketBusyPoll = %v, want %v", got, d)
		}
		c := Config{Resources: r}.WithDefaults().WithDefaults()
		if got := c.Resources.Resolve().SocketBusyPoll; got != d {
			t.Errorf("after WithDefaults, SocketBusyPoll resolves to %v, want %v", got, d)
		}
	}
}

func TestValidateSocketBusyPoll(t *testing.T) {
	maxOK := time.Duration(math.MaxInt32) * time.Microsecond
	for _, tc := range []struct {
		name string
		d    time.Duration
		bad  bool
	}{
		{"zero is off", 0, false},
		{"one microsecond", time.Microsecond, false},
		{"fifty microseconds", 50 * time.Microsecond, false},
		{"the largest the kernel takes", maxOK, false},
		{"negative", -time.Microsecond, true},
		{"minus one is no sentinel", -1, true},
		{"very negative", -time.Hour, true},
		{"sub-microsecond would truncate to off", 500 * time.Nanosecond, true},
		{"one nanosecond", 1, true},
		{"past the kernel's int", maxOK + time.Microsecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var found error
			for _, err := range (Config{Resources: Resources{SocketBusyPoll: tc.d}}).Validate() {
				if strings.Contains(err.Error(), "socketBusyPoll") {
					found = err
				}
			}
			switch {
			case tc.bad && found == nil:
				t.Errorf("SocketBusyPoll %v: Validate accepted it", tc.d)
			case !tc.bad && found != nil:
				t.Errorf("SocketBusyPoll %v: Validate rejected it: %v", tc.d, found)
			}
		})
	}
}
