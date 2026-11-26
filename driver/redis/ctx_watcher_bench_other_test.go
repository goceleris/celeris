//go:build !linux

package redis_test

import (
	"testing"

	"github.com/goceleris/celeris/driver/redis"
)

type benchMode = struct {
	name string
	opts []redis.Option
}

func benchPlatformModes(*testing.B) []benchMode { return nil }
