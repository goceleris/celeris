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

// failFlag is clusterNode.failing: a flag that also remembers whether it was last
// set by a failed PubSub dial. One atomic word holds both bits, so every change
// is one atomic operation and none can be lost to another:
//
//	0           healthy
//	failingBit  failing, set by the command path or found by a topology refresh
//	failingBit|byPubSubBit  failing, set by a failed PubSub dial
//
// Store, which the command path and the topology refresh use, takes the node
// over: it replaces the word, so a later successful PubSub dial leaves it alone.
// markPubSub replaces it with the PubSub's own verdict. clearPubSub clears the
// flag only if the word is exactly the PubSub's own mark (one compare and
// swap), so it never erases a mark that changed after it looked.
type failFlag struct {
	w atomic.Uint32
}

const (
	failingBit  uint32 = 1
	byPubSubBit uint32 = 2
)

// Load reports whether the node is marked failing.
func (f *failFlag) Load() bool { return f.w.Load()&failingBit != 0 }

// Store sets the flag as the command path or the topology refresh found it.
func (f *failFlag) Store(v bool) {
	if v {
		f.w.Store(failingBit)
		return
	}
	f.w.Store(0)
}

// markPubSub sets the flag because a PubSub dial failed.
func (f *failFlag) markPubSub() { f.w.Store(failingBit | byPubSubBit) }

// clearPubSub clears the flag after a successful PubSub dial, but only if a
// failed PubSub dial set it and nothing has changed it since.
func (f *failFlag) clearPubSub() { f.w.CompareAndSwap(failingBit|byPubSubBit, 0) }

// Close tears down the node's client.
func (n *clusterNode) Close() error {
	if n.client != nil {
		return n.client.Close()
	}
	return nil
}
