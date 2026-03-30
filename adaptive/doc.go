//go:build linux

// Package adaptive implements a dual-engine controller that dynamically switches
// between io_uring and epoll based on runtime telemetry.
//
// The adaptive [Engine] starts both sub-engines on the same port (via SO_REUSEPORT)
// and periodically evaluates their performance scores. If the standby engine's
// historical score exceeds the active engine's score by more than a threshold,
// the controller triggers a switch. Oscillation detection locks switching for
// five minutes after three rapid switches.
//
// Users select the adaptive engine via celeris.Config{Engine: celeris.Adaptive}.
// It is the default engine on Linux.
package adaptive
