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
		"abc":       "abc",
		"100%off":   "100\\%off",
		"a_b":       "a\\_b",
		"back\\":    "back\\\\",
		"a%_\\":     "a\\%\\_\\\\",
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
