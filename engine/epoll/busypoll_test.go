//go:build linux

package epoll

import (
	"testing"
	"time"
)

func TestResolveBusyPoll(t *testing.T) {
	def := time.Duration(defaultEpollBusyPollUS) * time.Microsecond
	tests := []struct {
		name string
		env  string
		set  bool
		want time.Duration
	}{
		{name: "unset uses default", set: false, want: def},
		{name: "empty uses default", env: "", set: true, want: def},
		{name: "zero disables", env: "0", set: true, want: 0},
		{name: "explicit value", env: "100", set: true, want: 100 * time.Microsecond},
		{name: "negative uses default", env: "-5", set: true, want: def},
		{name: "malformed uses default", env: "x", set: true, want: def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(envEpollBusyPollUS, tt.env)
			} else {
				t.Setenv(envEpollBusyPollUS, "")
			}
			if got := resolveBusyPoll(); got != tt.want {
				t.Errorf("resolveBusyPoll() = %v, want %v", got, tt.want)
			}
		})
	}
}
