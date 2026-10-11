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
	// A host that is not four bytes bound a padded address ("1.2.3" bound
	// 1.2.3.0, "10" bound 10.0.0.0, "1..2.3" bound 1.0.2.3).
	for _, addr := range []string{"1.2.3:80", "10:80", ".1.2.3:80", "1..2.3:80", "1.2.3.:80", "1.2.3.4.:80", "...:80"} {
		if sa, err := parseAddr(addr); err == nil {
			t.Errorf("parseAddr(%q) = %v, want an error", addr, sa)
		}
	}
	// A byte written with a leading zero is ambiguous (octal in some parsers):
	// net.ParseIP and the std engine refuse it, so do these ("0127.0.0.1"
	// bound 127.0.0.1, "010.0.0.1" bound 10.0.0.1).
	for _, addr := range []string{"0127.0.0.1:80", "010.0.0.1:0", "1.02.3.4:80", "1.2.3.04:80", "0000000001.2.3.4:80", "00.1.2.3:80"} {
		if sa, err := parseAddr(addr); err == nil {
			t.Errorf("parseAddr(%q) = %v, want an error", addr, sa)
		}
	}
	// A port over 65535 wrapped: "1.2.3.4:18446744073709551616" read as port 0,
	// which Listen then took for a request for an ephemeral port.
	for _, addr := range []string{"1.2.3.4:65536", "1.2.3.4:99999", "1.2.3.4:18446744073709551616", ":99999999999999999999", "[::1]:65536"} {
		if sa, err := parseAddr(addr); err == nil {
			t.Errorf("parseAddr(%q) = %v, want an error", addr, sa)
		}
	}
	// A port written with leading zeros is a port, as net.Listen reads it.
	for _, addr := range []string{"1.2.3.4:0080", "1.2.3.4:65535", "0.0.0.0:0", "1.2.3.0:0", "10.0.0.0:80", "[::1]:65535"} {
		if _, err := parseAddr(addr); err != nil {
			t.Errorf("parseAddr(%q): %v", addr, err)
		}
	}
	for _, addr := range []string{"1.2.3.4:80", "255.255.255.255:0", "0.0.0.0:0", ":80", "10.0.0.1:0"} {
		if _, err := parseAddr(addr); err != nil {
			t.Errorf("parseAddr(%q): %v", addr, err)
		}
	}
}
