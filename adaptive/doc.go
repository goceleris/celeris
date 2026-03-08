//go:build linux

// Package adaptive implements a dual-engine controller that dynamically switches
// between io_uring and epoll based on runtime telemetry.
package adaptive
