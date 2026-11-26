//go:build !linux

package redis

import "testing"

// harshEngineOptions: no engine-backed mode off Linux.
func harshEngineOptions(t *testing.T) []Option { return nil }
