package postgres

import (
	"strings"
	"testing"
)

// The direct-mode gate refuses LISTEN and UNLISTEN (nothing could receive a
// notification) and lets NOTIFY through (it only sends).
func TestListenGateIsListenAndUnlistenOnly(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want bool
	}{
		{"LISTEN ch", true},
		{"listen ch", true},
		{"  /* c */ LISTEN ch;", true},
		{"-- c\nUNLISTEN ch", true},
		{"UNLISTEN *", true},
		{"NOTIFY ch", false},
		{"NOTIFY ch, 'payload'", false},
		{"notify ch;", false},
		{"SELECT pg_notify('ch', 'p')", false},
		{"LISTENABLE", false},
		{"SELECT 1", false},
	} {
		if got := isListenOrUnlisten(tc.q); got != tc.want {
			t.Errorf("isListenOrUnlisten(%q) = %v, want %v", tc.q, got, tc.want)
		}
	}
}

func TestErrDirectModeUnsupportedNamesTheWayBack(t *testing.T) {
	for _, want := range []string{"LISTEN", "UNLISTEN", "Connector.WithStreaming", "streaming=true"} {
		if !strings.Contains(ErrDirectModeUnsupported.Error(), want) {
			t.Errorf("%q does not name %q", ErrDirectModeUnsupported, want)
		}
	}
	if strings.Contains(ErrDirectModeUnsupported.Error(), "NOTIFY") {
		t.Errorf("%q still names NOTIFY, which works", ErrDirectModeUnsupported)
	}
}
