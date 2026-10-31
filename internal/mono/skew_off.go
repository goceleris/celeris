//go:build !celeris_monoskew

package mono

// skew is the skew canary's offset (see the package documentation): none in a
// normal build, so the compiler removes every use of it.
const skew int64 = 0
