// Package redisspec contains a Redis RESP2/RESP3 protocol compliance
// verification suite. It speaks raw TCP to a Redis server and validates wire
// framing, type encoding, and state transitions against the Redis protocol
// specification (https://redis.io/docs/reference/protocol-spec/).
//
// The suite is organised by spec section:
//
//   - Section 1: RESP2 types (+, -, :, $, *)
//   - Section 2: RESP3 types (_, #, ',', (, !, =, ~, %, |, >)
//   - Section 3: Command protocol (inline, multibulk, pipeline, edge cases)
//   - Section 4: AUTH + SELECT
//   - Section 5: Pub/Sub protocol
//   - Section 6: Transaction protocol (MULTI/EXEC)
//   - Section 7: Wire edge cases (split reads, flooding, binary keys, concurrency)
//   - Section 8: Protocol fuzzing (fuzz_test.go)
//
// # Running
//
// The tests are gated by the `redisspec` build tag and the CELERIS_REDIS_ADDR
// environment variable. Both must be present for any test to execute:
//
//	CELERIS_REDIS_ADDR='127.0.0.1:6379' \
//	  go test -tags redisspec -count=1 -timeout=300s -v ./test/redisspec/...
//
// A default Redis 7.2 instance with no authentication is sufficient for all
// tests except the AUTH section, which requires CELERIS_REDIS_PASSWORD.
//
// # Design
//
// Every test uses raw TCP connections and the celeris
// [github.com/goceleris/celeris/driver/redis/protocol] package for RESP
// encoding/decoding. No third-party Redis client library is imported. This
// isolates the verification to pure protocol-level correctness.
package redisspec
