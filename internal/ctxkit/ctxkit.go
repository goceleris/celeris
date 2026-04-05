// Package ctxkit provides a single internal hook for releasing celeris
// contexts from packages that cannot import the root celeris package
// (e.g. internal/conn) due to circular dependencies.
package ctxkit

// ReleaseContext is registered by the celeris package at init time.
// internal/conn/h1.go uses it to release cached contexts on connection close.
var ReleaseContext func(c any)
