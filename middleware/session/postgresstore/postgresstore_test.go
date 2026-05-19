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
