package singleflight

import "testing"

// benchHeaders is a typical API response's header set as a leader hands it
// to its waiters (celeris#742): some echo request headers.
var benchHeaders = [][2]string{
	{"content-type", "application/json"},
	{"cache-control", "no-store"},
	{"x-request-id", "0f8c2a3e-6b1d-4c9e-9a57-3d2e1f0b7c64"},
	{"vary", "Origin"},
	{"access-control-allow-origin", "https://app.example.com"},
	{"x-ratelimit-remaining", "99"},
}

var sinkHeaders [][2]string

// BenchmarkWaiterCaptureSliceCopy742 is the capture the leader did before:
// the header slice copied, its strings shared.
func BenchmarkWaiterCaptureSliceCopy742(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		h := make([][2]string, len(benchHeaders))
		copy(h, benchHeaders)
		sinkHeaders = h
	}
}

// BenchmarkWaiterCaptureOwned742 is the capture now: ownHeaders copies the
// strings too. Either runs only when a waiter joined the leader.
func BenchmarkWaiterCaptureOwned742(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkHeaders, _ = ownHeaders(benchHeaders, "application/json")
	}
}
