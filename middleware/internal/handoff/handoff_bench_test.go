package handoff_test

import (
	"errors"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/handoff"
)

// The cost of handing a leader's error to its waiters (celeris#732), paid
// once per leader that waiters joined and whose handler failed, not per
// request.
func BenchmarkHandoffError(b *testing.B) {
	err := errors.New("user-0123456789 not found")
	b.ReportAllocs()
	for b.Loop() {
		_ = handoff.Error(err)
	}
}

func BenchmarkHandoffHTTPError(b *testing.B) {
	err := celeris.NewHTTPError(404, "user-0123456789 not found")
	b.ReportAllocs()
	for b.Loop() {
		_ = handoff.Error(err)
	}
}
