package engine

import "testing"

func TestProtocolString(t *testing.T) {
	tests := []struct {
		p    Protocol
		want string
	}{
		{HTTP1, "http1"},
		{H2C, "h2c"},
		{Auto, "auto"},
		{Protocol(255), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("Protocol(%d).String() = %q, want %q", tt.p, got, tt.want)
		}
	}
}
