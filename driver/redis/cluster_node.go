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
	// command or topology refresh confirms it is alive.
	failing atomic.Bool
	// psFailed is set when a PubSub dial on this node failed (and set failing
	// with it); a later successful PubSub dial clears failing only if psFailed
	// was set, so it never undoes the command path's verdict.
	psFailed atomic.Bool
	// latency stores the last-measured round-trip time in nanoseconds, updated
	// during topology refresh. Used by RouteByLatency to pick the fastest node.
	latency atomic.Int64
}

// Close tears down the node's client.
func (n *clusterNode) Close() error {
	if n.client != nil {
		return n.client.Close()
	}
	return nil
}
