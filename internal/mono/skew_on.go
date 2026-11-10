//go:build celeris_monoskew

package mono

import (
	"fmt"
	"os"
	"time"
)

// skew is the skew canary's offset: CELERIS_MONO_SKEW as a Go duration
// ("3600s", "-1h"), read once at process start. Built only with
// -tags celeris_monoskew, which only test runs pass.
var skew = skewFromEnv()

func skewFromEnv() int64 {
	s := os.Getenv("CELERIS_MONO_SKEW")
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(fmt.Sprintf("mono: CELERIS_MONO_SKEW=%q: %v", s, err))
	}
	return int64(d)
}
