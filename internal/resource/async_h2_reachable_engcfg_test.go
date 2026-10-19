package resource

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

// TestAsyncH2Reachable is the table behind the engines' AsyncHandlers note
// (checklist #885 item 26, #884 item 44). Two wrong gates are pinned by name:
// the old one, EnableH2Upgrade alone, and the one the checklist proposed,
// Protocol != HTTP1; each row says which of them it separates from the right
// one.
func TestAsyncH2Reachable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		async   bool
		proto   engine.Protocol
		upgrade bool
		want    bool
	}{
		{"auto, upgrade on", true, engine.Auto, true, true},
		{"auto, upgrade off (prior-knowledge h2c; the old gate misses it)", true, engine.Auto, false, true},
		{"h2c, upgrade on", true, engine.H2C, true, true},
		{"h2c, upgrade off (prior-knowledge h2c; the old gate misses it)", true, engine.H2C, false, true},
		{"unset protocol is auto", true, 0, false, true},
		{"http1, upgrade on (upgrades; the Protocol != HTTP1 gate misses it)", true, engine.HTTP1, true, true},
		{"http1, upgrade off: no HTTP/2 at all", true, engine.HTTP1, false, false},
		{"auto, upgrade off, not async", false, engine.Auto, false, false},
		{"h2c, upgrade on, not async", false, engine.H2C, true, false},
		{"http1, upgrade on, not async", false, engine.HTTP1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{AsyncHandlers: tc.async, Protocol: tc.proto}
			c.SetH2Upgrade(tc.upgrade)
			if got := c.AsyncH2Reachable(); got != tc.want {
				t.Errorf("Config{AsyncHandlers: %v, Protocol: %v, EnableH2Upgrade: %v}.AsyncH2Reachable() = %v, want %v",
					tc.async, tc.proto, tc.upgrade, got, tc.want)
			}
		})
	}
}

// TestAsyncH2NoticeNamesHTTP2 pins the note's wording: it says HTTP/2 (it
// used to say "H2" and name EnableH2Upgrade, which is one way in, not the
// condition).
func TestAsyncH2NoticeNamesHTTP2(t *testing.T) {
	if !strings.Contains(AsyncH2Notice, "HTTP/2") || strings.Contains(AsyncH2Notice, "EnableH2Upgrade") {
		t.Errorf("AsyncH2Notice = %q: it must name HTTP/2 and not the upgrade flag", AsyncH2Notice)
	}
}
