package redis

import (
	"errors"
	"fmt"
)

// ErrNil is returned by commands whose reply is a null bulk — e.g. GET on a
// missing key, LPOP on an empty list, or HGET on a missing field.
var ErrNil = errors.New("celeris-redis: nil")

// ErrClosed is returned when a command is issued against a closed Client or
// PubSub.
var ErrClosed = errors.New("celeris-redis: client closed")

// ErrProtocol is returned when the server reply cannot be decoded.
var ErrProtocol = errors.New("celeris-redis: protocol error")

// ErrMoved is returned when the server responds with a -MOVED redirect.
// When using [ClusterClient], MOVED redirects are handled transparently; this
// sentinel is still useful for errors.Is checks on single-node [Client].
var ErrMoved = errors.New("celeris-redis: MOVED redirect")

// ErrAsk is returned when the server responds with an -ASK redirect during
// a cluster slot migration.
var ErrAsk = errors.New("celeris-redis: ASK redirect")

// ErrWrongType matches the server's WRONGTYPE error reply for operations
// against a key of the wrong data type.
var ErrWrongType = errors.New("celeris-redis: WRONGTYPE operation against a key holding the wrong kind of value")

// ErrPoolExhausted is returned when no connection is available and none can
// be dialed within MaxOpen.
var ErrPoolExhausted = errors.New("celeris-redis: pool exhausted")

// ErrTxAborted is returned from each typed *Cmd.Result() after a MULTI/EXEC
// transaction aborts — e.g. because a WATCHed key was modified between the
// WATCH call and EXEC, or the server rejected the transaction with
// -EXECABORT.
var ErrTxAborted = errors.New("celeris-redis: transaction aborted")

// ErrCrossSlot is returned by [ClusterTx] and [ClusterClient.Watch] when the
// queued commands (or watched keys) span multiple hash slots. Redis Cluster
// requires all keys in a MULTI/EXEC transaction to reside in the same slot.
// Use hash tags (e.g. "{tag}") to colocate related keys.
var ErrCrossSlot = errors.New("celeris-redis: CROSSSLOT keys in request don't hash to the same slot; use hash tags {}")

// RedisError wraps a server-side error reply. The Prefix (e.g. "WRONGTYPE",
// "ERR", "MOVED") distinguishes categories; the Msg holds the full text.
type RedisError struct {
	Prefix string
	Msg    string
}

// Error implements the error interface.
func (e *RedisError) Error() string {
	if e.Prefix == "" {
		return "celeris-redis: " + e.Msg
	}
	return fmt.Sprintf("celeris-redis: %s %s", e.Prefix, e.Msg)
}

// Is supports errors.Is for well-known prefixes.
func (e *RedisError) Is(target error) bool {
	switch target {
	case ErrMoved:
		return e.Prefix == "MOVED"
	case ErrAsk:
		return e.Prefix == "ASK"
	case ErrWrongType:
		return e.Prefix == "WRONGTYPE"
	}
	return false
}

// parseError splits a RESP simple/blob error body ("PREFIX message...") into
// prefix and message. The prefix is the first whitespace-delimited token.
func parseError(body []byte) *RedisError {
	s := string(body)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return &RedisError{Prefix: s[:i], Msg: s[i+1:]}
		}
	}
	return &RedisError{Msg: s}
}
