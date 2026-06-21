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
// Tests are gated by the `redisspec` build tag and the CELERIS_REDIS_ADDR
// environment variable. Every test uses raw TCP connections and the
// [github.com/goceleris/celeris/driver/redis/protocol] package for RESP
// encoding/decoding — no third-party Redis client is imported.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs
package redisspec
