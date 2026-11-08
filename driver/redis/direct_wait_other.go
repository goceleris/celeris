//go:build !linux

package redis

// directBlockSupported is false off linux: every wait parks in Go's netpoll,
// whatever the mode (celeris#854).
const directBlockSupported = false
