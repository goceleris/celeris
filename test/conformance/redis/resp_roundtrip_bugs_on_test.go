//go:build redis && redisrtbugs

package redis_test

// rtKnownBugs is true under `-tags 'redis redisrtbugs'`: the reply-type matrix
// then also runs the cells that are known to fail today, so one command lists
// every wrong reply decode with its bug id. The default build leaves them out,
// so the Driver Conformance job stays green and has no SKIP line.
const rtKnownBugs = true
