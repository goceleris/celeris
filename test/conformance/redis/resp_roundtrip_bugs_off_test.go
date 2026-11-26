//go:build redis && !redisrtbugs

package redis_test

// rtKnownBugs is false in the default build: cells that fail today are not run
// (and not skipped, a skip is not coverage). See resp_roundtrip_bugs_on_test.go.
const rtKnownBugs = false
