//go:build redis && !linux

package redis_test

import (
	"testing"

	"github.com/goceleris/celeris"
	celredis "github.com/goceleris/celeris/driver/redis"
)

func stressPlatformModes(t *testing.T) map[string][]celredis.Option {
	return map[string][]celredis.Option{
		"engine-not-started-std (direct)": {celredis.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std}))},
	}
}
