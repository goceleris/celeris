// Package ctxkit provides internal hooks for creating and releasing
// celeris contexts from the celeristest package without exposing
// implementation types in the public API.
package ctxkit

import (
	"net"
	"time"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

// Hooks registered by the celeris package at init time.
var (
	NewContext        func(s *stream.Stream) any
	ReleaseContext    func(c any)
	AddParam          func(c any, key, value string)
	SetHandlers       func(c any, handlers []any)
	GetResponseWriter func(c any) any
	GetStream         func(c any) any
	SetStartTime      func(c any, t time.Time)
	SetFullPath       func(c any, path string)
	SetTrustedNets    func(c any, nets []*net.IPNet)
	SetProtoMajor     func(c any, v uint8)
)
