package redis

import "sync/atomic"

// clusterNode represents a single node in a Redis Cluster. Each node owns its
// own Client (with its own connection pool) and tracks whether it is a primary
// or replica.
type clusterNode struct {
	addr    string
	client  *Client
	primary bool
	// failing is set when the node is unreachable; cleared after a successful
	// command or topology refresh confirms it is alive. A PubSub dial also
	// sets it, and clears only what it set (see failFlag).
	failing failFlag
	// latency stores the last-measured round-trip time in nanoseconds, updated
	// during topology refresh. Used by RouteByLatency to pick the fastest node.
	latency atomic.Int64
}

// failFlag is clusterNode.failing: a flag that remembers whether the last
// change came from a PubSub dial. Store, which the command path and the topology
// refresh use, takes the node over: a later successful PubSub dial then leaves
// the flag alone. markPubSub and clearPubSub are the PubSub path's.
type failFlag struct {
	atomic.Bool
	pubsub atomic.Bool
}

// Store sets the flag as the command path or the topology refresh found it.
func (f *failFlag) Store(v bool) {
	f.pubsub.Store(false)
	f.Bool.Store(v)
}

// markPubSub sets the flag because a PubSub dial failed.
func (f *failFlag) markPubSub() {
	f.Bool.Store(true)
	f.pubsub.Store(true)
}

// clearPubSub clears the flag after a successful PubSub dial, but only if the
// last thing that changed it was a failed PubSub dial.
func (f *failFlag) clearPubSub() {
	if f.pubsub.CompareAndSwap(true, false) {
		f.Bool.Store(false)
	}
}

// Close tears down the node's client.
func (n *clusterNode) Close() error {
	if n.client != nil {
		return n.client.Close()
	}
	return nil
}
