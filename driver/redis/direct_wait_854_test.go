package redis

import "testing"

// TestParseDirectWait854 pins the CELERIS_REDIS_DIRECT_WAIT values: auto and
// block, in any case and with spaces around, and netpoll for everything else.
// An unknown value never switches the feature on.
func TestParseDirectWait854(t *testing.T) {
	for in, want := range map[string]directWaitKind{
		"": directWaitNetpoll, "netpoll": directWaitNetpoll, "NetPoll": directWaitNetpoll,
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
// with the environment. Unset means netpoll.
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
	if got := currentDirectWait(); got != directWaitNetpoll {
		t.Fatalf("mode with the variable empty = %d, want netpoll (%d)", got, directWaitNetpoll)
	}

	resetDirectWaitForTest(t)
	t.Setenv(directWaitEnv, "Yes please")
	if got := currentDirectWait(); got != directWaitNetpoll {
		t.Fatalf("mode with an unknown value = %d, want netpoll (%d)", got, directWaitNetpoll)
	}
}
