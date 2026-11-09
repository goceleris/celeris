package redis

import (
	"os"
	"runtime"
	"testing"
)

// wantDefaultDirectWait is the mode the environment not naming one must give,
// spelled out per platform and not taken from the code under test: the default
// (auto for the pools it was measured on) on linux, where the poll(2) wait
// exists, and netpoll everywhere else.
func wantDefaultDirectWait() directWaitKind {
	if runtime.GOOS == "linux" {
		return directWaitDefault
	}
	return directWaitNetpoll
}

// TestParseDirectWait854 pins the CELERIS_REDIS_DIRECT_WAIT values: netpoll,
// auto and block, in any case and with spaces around; empty or blank is the
// default (auto on linux for the measured pools, netpoll elsewhere); any other
// value falls back to netpoll, the pre-#854 path.
func TestParseDirectWait854(t *testing.T) {
	def := wantDefaultDirectWait()
	for in, want := range map[string]directWaitKind{
		"": def, " ": def, "\t\n": def,
		"netpoll": directWaitNetpoll, "NetPoll": directWaitNetpoll, " NETPOLL ": directWaitNetpoll,
		"auto": directWaitAuto, "Auto": directWaitAuto, " auto\n": directWaitAuto,
		"block": directWaitBlock, "BLOCK": directWaitBlock, "\tBlock ": directWaitBlock,
		"off": directWaitNetpoll, "0": directWaitNetpoll, "1": directWaitNetpoll,
		"true": directWaitNetpoll, "junk": directWaitNetpoll, "au to": directWaitNetpoll,
	} {
		if got := parseDirectWait(in); got != want {
			t.Errorf("parseDirectWait(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestDirectWaitEnvIsReadOnFirstUse854: the variable is read when the first
// Exec needs the mode and not before, so a program that sets it after the
// package is initialised still gets it; once read, the mode does not change
// with the environment. Unset or empty means the platform default.
func TestDirectWaitEnvIsReadOnFirstUse854(t *testing.T) {
	resetDirectWaitForTest(t)
	t.Setenv(directWaitEnv, " Block ") // set after "init", before the first use
	if got := currentDirectWait(); got != directWaitBlock {
		t.Fatalf("mode = %d, want block (%d)", got, directWaitBlock)
	}
	t.Setenv(directWaitEnv, "auto")
	if got := currentDirectWait(); got != directWaitBlock {
		t.Fatalf("mode = %d after the environment changed, want it read once (block)", got)
	}

	resetDirectWaitForTest(t)
	t.Setenv(directWaitEnv, "")
	if got, want := currentDirectWait(), wantDefaultDirectWait(); got != want {
		t.Fatalf("mode with the variable empty = %d, want the platform default (%d)", got, want)
	}

	resetDirectWaitForTest(t)
	t.Setenv(directWaitEnv, "Yes please")
	if got := currentDirectWait(); got != directWaitNetpoll {
		t.Fatalf("mode with an unknown value = %d, want netpoll (%d)", got, directWaitNetpoll)
	}
}

// TestDirectWaitDefault854: with the variable not set at all, or set to
// nothing, the mode is the default on linux (celeris#854) and netpoll
// elsewhere; an explicit netpoll is netpoll on every platform (the opt-out); an
// unknown value is netpoll.
func TestDirectWaitDefault854(t *testing.T) {
	resetDirectWaitForTest(t)
	t.Setenv(directWaitEnv, "")
	if err := os.Unsetenv(directWaitEnv); err != nil { // restored by t.Setenv's cleanup
		t.Fatal(err)
	}
	if got, want := currentDirectWait(), wantDefaultDirectWait(); got != want {
		t.Fatalf("mode with the variable unset = %d, want %d", got, want)
	}

	for _, c := range []struct {
		name, val string
		want      directWaitKind
	}{
		{"explicit netpoll", "netpoll", directWaitNetpoll},
		{"explicit auto", "auto", directWaitAuto},
		{"explicit block", "block", directWaitBlock},
		{"unknown", "autoo", directWaitNetpoll},
	} {
		resetDirectWaitForTest(t)
		t.Setenv(directWaitEnv, c.val)
		if got := currentDirectWait(); got != c.want {
			t.Errorf("%s (%q): mode = %d, want %d", c.name, c.val, got, c.want)
		}
	}
}

// TestConnDirectWaitResolvesTheDefaultPerConn854: the default resolves to auto
// on a conn of a measured pool and to netpoll on one whose pool opted out of
// the default (the WithEngine fallback, an unbounded pool); an explicit mode
// applies to both.
func TestConnDirectWaitResolvesTheDefaultPerConn854(t *testing.T) {
	for _, c := range []struct {
		name   string
		mode   directWaitKind
		optOut bool
		want   directWaitKind
	}{
		{"default", directWaitDefault, false, directWaitAuto},
		{"default, opted out", directWaitDefault, true, directWaitNetpoll},
		{"auto, opted out", directWaitAuto, true, directWaitAuto},
		{"block, opted out", directWaitBlock, true, directWaitBlock},
		{"netpoll", directWaitNetpoll, false, directWaitNetpoll},
		{"netpoll, opted out", directWaitNetpoll, true, directWaitNetpoll},
	} {
		t.Run(c.name, func(t *testing.T) {
			setDirectWaitForTest(t, c.mode)
			conn := &redisConn{netpollByDefault: c.optOut}
			if got := conn.directWait(); got != c.want {
				t.Fatalf("directWait() = %d, want %d", got, c.want)
			}
		})
	}
}
