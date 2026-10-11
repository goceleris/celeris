//go:build linux

package epoll

import "testing"

// parseAddr takes Config.Addr verbatim, and Config.Validate accepts any
// host:port. A host with a fourth dot used to index past the four address
// bytes and panic, in a loop's goroutine (or, since celeris#895, in Listen's);
// it is an error, as io_uring's parseAddr has always made it.
func TestParseAddrRejectsAFifthByte895(t *testing.T) {
	for _, addr := range []string{"1.2.3.4.5:80", "1.2.3.4.5.6.7:0", "1.2.3.4.:80"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("parseAddr(%q) panicked: %v", addr, r)
				}
			}()
			if sa, err := parseAddr(addr); err == nil {
				t.Errorf("parseAddr(%q) = %v, want an error", addr, sa)
			}
		}()
	}
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
