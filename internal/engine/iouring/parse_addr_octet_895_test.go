//go:build linux

package iouring

import "testing"

// parseAddr takes Config.Addr verbatim, and Config.Validate accepts any
// host:port. A byte over 255 used to wrap and bind another address. (The
// epoll twin of this file also covers a fourth dot, which only epoll's
// parseAddr indexed past the array on.)
func TestParseAddrRejectsABadByte895(t *testing.T) {
	// A byte over 255 used to wrap ("300.1.1.1" bound 44.1.1.1).
	for _, addr := range []string{"300.1.1.1:80", "1.2.3.256:0", "1.2.3.4000:80", "99999999999999999999.1.1.1:80"} {
		if sa, err := parseAddr(addr); err == nil {
			t.Errorf("parseAddr(%q) = %v, want an error", addr, sa)
		}
	}
	for _, addr := range []string{"1.2.3.4:80", "255.255.255.255:0", "0.0.0.0:0", ":80", "10.0.0.1:0"} {
		if _, err := parseAddr(addr); err != nil {
			t.Errorf("parseAddr(%q): %v", addr, err)
		}
	}
}
