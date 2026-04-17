package memcached

import (
	"errors"
	"fmt"
)

// ErrCacheMiss is returned by Get/GetBytes/Touch/CAS when the requested key
// does not exist on the server. Matches the "NOT_FOUND\r\n" text reply and
// the StatusKeyNotFound binary status.
var ErrCacheMiss = errors.New("celeris/memcached: cache miss")

// ErrNotStored is returned by Add/Replace/Append/Prepend when the server
// declines to store the value — Add finds the key already present, Replace /
// Append / Prepend find it missing, or the Append/Prepend target is a counter
// rather than a string.
var ErrNotStored = errors.New("celeris/memcached: item not stored")

// ErrCASConflict is returned by CAS when the server's stored CAS token no
// longer matches the caller-supplied one (concurrent modification).
var ErrCASConflict = errors.New("celeris/memcached: CAS conflict")

// ErrClosed is returned when a command is issued against a closed [Client].
var ErrClosed = errors.New("celeris/memcached: client closed")

// ErrProtocol is returned when the server reply cannot be decoded.
var ErrProtocol = errors.New("celeris/memcached: protocol error")

// ErrPoolExhausted is returned when no connection is available and none can
// be dialed within MaxOpen.
var ErrPoolExhausted = errors.New("celeris/memcached: pool exhausted")

// ErrMalformedKey is returned before wire contact when a key violates the
// text-protocol constraints (empty, too long, or contains whitespace/control
// bytes). The binary protocol has no such restrictions, but the client
// rejects the same set uniformly so a caller can switch dialects without
// changing their key-hygiene assumptions.
var ErrMalformedKey = errors.New("celeris/memcached: malformed key")

// ErrNoNodes is returned when a [ClusterClient] has no live nodes configured
// (typically because every ClusterConfig.Addrs entry was empty or all seed
// dials failed).
var ErrNoNodes = errors.New("celeris/memcached: cluster has no nodes")

// MemcachedError wraps a server-side error reply.
//
// For text protocol it carries the Kind tag ("ERROR", "CLIENT_ERROR",
// "SERVER_ERROR") and the accompanying message. For binary protocol it
// carries the raw Status code and the response body (usually a human
// message).
type MemcachedError struct {
	Kind   string
	Status uint16
	Msg    string
}

// Error implements the error interface.
func (e *MemcachedError) Error() string {
	if e.Kind != "" {
		if e.Msg == "" {
			return fmt.Sprintf("celeris/memcached: %s", e.Kind)
		}
		return fmt.Sprintf("celeris/memcached: %s %s", e.Kind, e.Msg)
	}
	return fmt.Sprintf("celeris/memcached: status=0x%04x %s", e.Status, e.Msg)
}
