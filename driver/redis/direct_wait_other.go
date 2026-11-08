//go:build !linux

package redis

import (
	"context"
	"time"
)

// directBlockSupported is false off linux: every wait parks in Go's netpoll,
// whatever the mode (celeris#854).
const directBlockSupported = false

// directPollIn is never called (see directBlockSupported).
func directPollIn(context.Context, sockFD, time.Time, bool) bool { return false }
