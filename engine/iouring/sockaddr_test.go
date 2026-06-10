//go:build linux

package iouring

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestSockaddrString verifies the allocation-light sockaddrString formatter
// (v1.5.0 review 2.11) produces the same canonical output as the prior
// fmt.Sprintf implementation.
func TestSockaddrString(t *testing.T) {
	tests := []struct {
		name string
		sa   unix.Sockaddr
		want string
	}{
		{"ipv4", &unix.SockaddrInet4{Addr: [4]byte{192, 168, 1, 50}, Port: 8080}, "192.168.1.50:8080"},
		{"ipv4-zero", &unix.SockaddrInet4{Addr: [4]byte{0, 0, 0, 0}, Port: 0}, "0.0.0.0:0"},
		{"ipv4-max", &unix.SockaddrInet4{Addr: [4]byte{255, 255, 255, 255}, Port: 65535}, "255.255.255.255:65535"},
		{"ipv6-loopback", &unix.SockaddrInet6{Addr: [16]byte{15: 1}, Port: 443}, "[::1]:443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sockaddrString(tt.sa); got != tt.want {
				t.Errorf("sockaddrString = %q, want %q", got, tt.want)
			}
		})
	}
}

func BenchmarkSockaddrStringIPv4(b *testing.B) {
	sa := &unix.SockaddrInet4{Addr: [4]byte{10, 0, 12, 200}, Port: 54321}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sockaddrString(sa)
	}
}
