package celeris

import (
	"testing"
	"time"
)

// celeris#1228: Server.SocketBusyPoll is what a driver opened WithEngine(srv)
// takes as its SO_BUSY_POLL budget. It is the resolved Config.SocketBusyPoll of
// a native engine, 0 (off) by default, and 0 for the std engine, which ignores
// the option. It does not need the server to be started.
func TestServerSocketBusyPoll(t *testing.T) {
	const d = 37 * time.Microsecond
	for _, tc := range []struct {
		name string
		cfg  Config
		want time.Duration
	}{
		{"default engine, unset", Config{}, 0},
		{"epoll, unset", Config{Engine: Epoll}, 0},
		{"epoll", Config{Engine: Epoll, SocketBusyPoll: d}, d},
		{"io_uring", Config{Engine: IOUring, SocketBusyPoll: d}, d},
		{"adaptive", Config{Engine: Adaptive, SocketBusyPoll: d}, d},
		{"std ignores it", Config{Engine: Std, SocketBusyPoll: d}, 0},
		{"negative is off", Config{Engine: Epoll, SocketBusyPoll: -d}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := New(tc.cfg).SocketBusyPoll(); got != tc.want {
				t.Errorf("SocketBusyPoll() = %v, want %v", got, tc.want)
			}
		})
	}
}
