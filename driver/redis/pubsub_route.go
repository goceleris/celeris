package redis

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// pubsubRoute chooses the client a PubSub's next conn is dialled on, so a
// subscription follows its node when the node is gone and its slot when the
// slot moves (celeris#887 item 31). A PubSub of a standalone Client has none:
// it dials that client for ever.
//
// pick and dialed run on the PubSub's reconnect goroutine with no PubSub lock
// held.
type pubsubRoute interface {
	// pick returns the client to dial. shard is a shard channel the PubSub
	// holds ("" if none): the client must be the owner of its slot. cur is
	// the client the PubSub last used and moveOn is true when it did not work
	// out; for a subscription that can live on any node a route then returns
	// another. An error that matches [ErrClosed] means the route's owner is
	// closed and nothing will ever work (the loop gives up); any other error is
	// retried with the backoff.
	pick(ctx context.Context, shard string, cur *Client, moveOn bool) (*Client, error)
	// dialed reports the outcome of a dial on cl.
	dialed(cl *Client, ok bool)
}

// errPubSubNoOwner is a transient pick failure: the topology names no node for
// the slot, or none is known.
var errPubSubNoOwner = errors.New("celeris-redis: no cluster node for the pubsub subscription")

// clusterPubSubRoute routes the PubSubs of a ClusterClient.
//
//   - A regular SUBSCRIBE or PSUBSCRIBE is node-local and the cluster bus
//     delivers every PUBLISH to every node, so any node will do: the PubSub
//     stays on its node while that works and otherwise goes to the next one
//     (by address) that is not marked failing.
//   - A sharded subscription lives on the owner of its slot: the topology is
//     refreshed (bounded by the dial timeout) and the owner of the slot read.
type clusterPubSubRoute struct{ c *ClusterClient }

func (r clusterPubSubRoute) pick(ctx context.Context, shard string, cur *Client, moveOn bool) (*Client, error) {
	c := r.c
	if c.closed.Load() {
		return nil, ErrClosed
	}
	if shard != "" {
		bound := c.cfg.DialTimeout
		if bound <= 0 {
			bound = defaultDialTimeout
		}
		rctx, cancel := context.WithTimeout(ctx, bound)
		// A failed refresh leaves the table as it was: the dial tells.
		_ = c.refreshTopology(rctx)
		cancel()
		if c.closed.Load() {
			return nil, ErrClosed
		}
		n := c.route(Slot(shard))
		if n == nil {
			// The table names no owner (the SSubscribe constructor does the
			// same): any node, which answers -MOVED if it is the wrong one,
			// and the PubSub asks again.
			n = c.anyNode()
		}
		if n == nil {
			return nil, errPubSubNoOwner
		}
		return n.client, nil
	}
	return c.pubsubNode(cur, moveOn)
}

func (r clusterPubSubRoute) dialed(cl *Client, ok bool) {
	c := r.c
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, n := range c.nodes {
		if n.client == cl {
			n.failing.Store(!ok)
			return
		}
	}
}

// pubsubNode picks the node of a regular subscription: cur while it works,
// otherwise the next one after it, by address, that is not marked failing (or
// the next one, if all are).
func (c *ClusterClient) pubsubNode(cur *Client, moveOn bool) (*Client, error) {
	c.mu.RLock()
	nodes := make([]*clusterNode, 0, len(c.nodes))
	for _, n := range c.nodes {
		nodes = append(nodes, n)
	}
	c.mu.RUnlock()
	if len(nodes) == 0 {
		if c.closed.Load() {
			return nil, ErrClosed
		}
		return nil, errPubSubNoOwner
	}
	slices.SortFunc(nodes, func(a, b *clusterNode) int { return strings.Compare(a.addr, b.addr) })
	at := -1
	for i, n := range nodes {
		if n.client == cur {
			at = i
		}
	}
	if !moveOn && at >= 0 && !nodes[at].failing.Load() {
		return cur, nil
	}
	for i := 1; i <= len(nodes); i++ {
		if n := nodes[(at+i+len(nodes))%len(nodes)]; !n.failing.Load() {
			return n.client, nil
		}
	}
	return nodes[(at+1+len(nodes))%len(nodes)].client, nil
}

// openPubSub opens the PubSub of a ClusterClient. It dials first and, if that
// node is down, the others in turn (a regular subscription) or the owner the
// refreshed topology names (a shard channel: shard is its first channel), so a
// node that is gone does not fail the call while another would serve it.
func (c *ClusterClient) openPubSub(ctx context.Context, first *Client, shard string) (*PubSub, error) {
	route := clusterPubSubRoute{c}
	cl := first
	var lastErr error
	for try := 0; ; try++ {
		ps, err := cl.newPubSubRouted(ctx, route)
		if err == nil {
			route.dialed(cl, true)
			return ps, nil
		}
		route.dialed(cl, false)
		lastErr = err
		c.mu.RLock()
		n := len(c.nodes)
		c.mu.RUnlock()
		if ctx.Err() != nil || try+1 >= max(n, 2) || c.closed.Load() {
			return nil, lastErr
		}
		next, perr := route.pick(ctx, shard, cl, true)
		if perr != nil {
			return nil, lastErr
		}
		cl = next
	}
}

// sentinelPubSubRoute routes the PubSubs of a SentinelClient: the current
// master, so a PubSub follows a failover instead of dialling the old master's
// closed client for ever.
type sentinelPubSubRoute struct{ s *SentinelClient }

func (r sentinelPubSubRoute) pick(context.Context, string, *Client, bool) (*Client, error) {
	return r.s.getClient()
}

func (r sentinelPubSubRoute) dialed(*Client, bool) {}
