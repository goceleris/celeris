//go:build linux

package iouring

import "testing"

func TestResolveSendZCMin(t *testing.T) {
	tests := []struct {
		name string
		env  string
		set  bool
		want int
	}{
		{name: "unset uses default", set: false, want: defaultSendZCMinBytes},
		{name: "empty uses default", env: "", set: true, want: defaultSendZCMinBytes},
		{name: "zero disables threshold", env: "0", set: true, want: 0},
		{name: "explicit value", env: "8192", set: true, want: 8192},
		{name: "negative uses default", env: "-1", set: true, want: defaultSendZCMinBytes},
		{name: "malformed uses default", env: "garbage", set: true, want: defaultSendZCMinBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(envSendZCMin, tt.env)
			} else {
				// Ensure the var is absent for this subtest.
				t.Setenv(envSendZCMin, "")
			}
			if got := resolveSendZCMin(); got != tt.want {
				t.Errorf("resolveSendZCMin() = %d, want %d", got, tt.want)
			}
		})
	}
}
