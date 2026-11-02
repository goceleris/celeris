package celeris

import (
	"strings"
	"testing"
	"time"
)

// SO_BUSY_POLL is opt-in (celeris#1036): Config.SocketBusyPoll
// reaches the engines through toResourceConfig, unset is off, and a value the
// kernel cannot take is a start error (prepare), not a silent no-op.

func TestSocketBusyPollWiring(t *testing.T) {
	if got := (Config{}).toResourceConfig().WithDefaults().Resources.Resolve().SocketBusyPoll; got != 0 {
		t.Errorf("an unset Config resolves SocketBusyPoll to %v, want 0 (off)", got)
	}
	const want = 37 * time.Microsecond
	rc := Config{SocketBusyPoll: want}.toResourceConfig().WithDefaults()
	if got := rc.Resources.Resolve().SocketBusyPoll; got != want {
		t.Errorf("Config.SocketBusyPoll %v resolves to %v through toResourceConfig", want, got)
	}
}

// TestSocketBusyPollRejectedValues goes through Server.prepare (what Start runs), so it also
// proves toResourceConfig hands the raw value to Validate (a negative one
// dropped by a "> 0" guard, as SocketRecvBuf's is, would pass).
func TestSocketBusyPollRejectedValues(t *testing.T) {
	for _, d := range []time.Duration{-1, -time.Microsecond, 100 * time.Nanosecond} {
		s := New(Config{Engine: Std, Addr: "127.0.0.1:0", SocketBusyPoll: d})
		_, err := s.prepare()
		if err == nil {
			t.Errorf("SocketBusyPoll %v: the server was prepared", d)
			continue
		}
		if !strings.Contains(err.Error(), "config validation") || !strings.Contains(err.Error(), "socketBusyPoll") {
			t.Errorf("SocketBusyPoll %v: error %q does not name config validation and socketBusyPoll", d, err)
		}
	}
}
