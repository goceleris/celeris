package websocket

import (
	"fmt"
	"net"
	"testing"
)

// TestPeerAddrParsesEngineForms pins the forms of the peer address the
// engines report that Conn.RemoteAddr turns into a *net.TCPAddr (celeris#721),
// the dual-stack form "[a.b.c.d]:port" (an IPv4 peer of a "[::]:port"
// listener) included: each must give the address the hijack path's net.Conn
// gives for that peer, which prints an IPv4-mapped address as IPv4.
func TestPeerAddrParsesEngineForms(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:41728", "*net.TCPAddr 127.0.0.1 41728 127.0.0.1:41728"},
		{"[::1]:80", "*net.TCPAddr ::1 80 [::1]:80"},
		{"[127.0.0.1]:35224", "*net.TCPAddr 127.0.0.1 35224 127.0.0.1:35224"},
		{"[::ffff:1.2.3.4]:4321", "*net.TCPAddr 1.2.3.4 4321 1.2.3.4:4321"},
		{"[fe80::1%eth0]:443", "*net.TCPAddr fe80::1 443 [fe80::1%eth0]:443"},
		{"not-an-address", "websocket.rawAddr not-an-address"},
		{"[127.0.0.1]:99999", "websocket.rawAddr [127.0.0.1]:99999"},
	} {
		var got string
		switch a := peerAddr(tc.in).(type) {
		case *net.TCPAddr:
			got = fmt.Sprintf("%T %s %d %s", a, a.IP, a.Port, a)
		default:
			got = fmt.Sprintf("%T %s", a, a)
		}
		if got != tc.want {
			t.Errorf("peerAddr(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if a := peerAddr(""); a != nil {
		t.Errorf("peerAddr(\"\") = %v, want nil", a)
	}
}
