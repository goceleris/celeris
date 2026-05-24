package postgresstore

import (
	"context"
	"testing"
)

func TestValidIdent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"celeris_sessions", true},
		{"Sessions", true},
		{"_x", true},
		{"a0", true},
		{"", false},
		{"0abc", false},
		{"a-b", false},
		{"a b", false},
		{"drop; --", false},
	}
	for _, c := range cases {
		if got := validIdent(c.in); got != c.want {
			t.Errorf("validIdent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"abc":     "abc",
		"100%off": "100\\%off",
		"a_b":     "a\\_b",
		"back\\":  "back\\\\",
		"a%_\\":   "a\\%\\_\\\\",
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewRejectsNilPool(t *testing.T) {
	_, err := New(context.Background(), nil)
	if err == nil {
		t.Fatal("New(nil) should error")
	}
}

func TestNewRejectsBadTableName(t *testing.T) {
	_, err := New(context.Background(), nil, Options{TableName: "drop; --"})
	if err == nil {
		t.Fatal("New with invalid table name should error")
	}
}

// TestPhaseHook_NilSafe pins the contract that emitPhase tolerates a
// nil hook (the production path — non-diagnostic deployments never
// set Options.PhaseHook). Underpins the v1.4.10 phase-logging feature.
func TestPhaseHook_NilSafe(t *testing.T) {
	s := &Store{} // phaseHook left zero
	// Must not panic.
	s.emitPhase("any-tag")
	s.emitPhase("another-tag")
}

// TestPhaseHook_EmittedInOrder pins the contract that emitPhase invokes
// the hook synchronously with the supplied tag — the order of tags
// observed by the hook reflects the order of emitPhase calls. This
// matters for diagnostic consumers (probatorium refapps) that print
// each phase to stderr to pinpoint where boot blocks.
func TestPhaseHook_EmittedInOrder(t *testing.T) {
	var phases []string
	s := &Store{phaseHook: func(p string) { phases = append(phases, p) }}
	s.emitPhase("a")
	s.emitPhase("b")
	s.emitPhase("c")
	want := []string{"a", "b", "c"}
	if len(phases) != len(want) {
		t.Fatalf("phases: got %v, want %v", phases, want)
	}
	for i, w := range want {
		if phases[i] != w {
			t.Errorf("phase[%d]: got %q, want %q", i, phases[i], w)
		}
	}
}

// TestAdvisoryLockKey_DeterministicPerTable pins the contract that
// advisoryLockKey is stable across calls for the same table (so two
// racers serialize on the same lock) and differs between tables (so
// unrelated stores don't block each other unnecessarily). Underpins
// the ensureSchema race fix.
func TestAdvisoryLockKey_DeterministicPerTable(t *testing.T) {
	tables := []string{
		"celeris_sessions",
		"celeris_sessions",
		"other_sessions",
		"yet_another",
	}
	keys := make(map[string]int64)
	for _, tbl := range tables {
		keys[tbl] = advisoryLockKey(tbl)
	}
	if keys["celeris_sessions"] == 0 {
		t.Fatal("advisoryLockKey returned zero — improbable, suggests broken hash")
	}
	if keys["celeris_sessions"] != advisoryLockKey("celeris_sessions") {
		t.Fatal("advisoryLockKey must be stable for the same input")
	}
	if keys["celeris_sessions"] == keys["other_sessions"] {
		t.Errorf("advisoryLockKey collision: %q and %q both → %d",
			"celeris_sessions", "other_sessions", keys["celeris_sessions"])
	}
	if keys["other_sessions"] == keys["yet_another"] {
		t.Errorf("advisoryLockKey collision: %q and %q both → %d",
			"other_sessions", "yet_another", keys["other_sessions"])
	}
}
