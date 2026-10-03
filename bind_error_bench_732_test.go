package celeris_test

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

// The cost of BindError's copy of Value (celeris#732): BindQuery of a value
// that does not convert, which now copies the value into the BindError, and
// of one that converts (the control: that path did not change).
func benchBindQuery732(b *testing.B, value string) {
	ctx, _ := celeristest.NewContext("GET", "/q", celeristest.WithQuery("n", value))
	defer celeristest.ReleaseContext(ctx)
	var v struct {
		N int `query:"n"`
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = ctx.BindQuery(&v)
	}
}

func BenchmarkBindQueryError732(b *testing.B) { benchBindQuery732(b, "aaaa") }

func BenchmarkBindQueryOK732(b *testing.B) { benchBindQuery732(b, "1234") }
