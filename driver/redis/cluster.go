package redis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/redis/protocol"
)

// readOnlyCommands is the set of Redis commands that are safe to route to
// replica nodes when ReadOnly or RouteByLatency is enabled.
var readOnlyCommands = map[string]bool{
	"GET": true, "MGET": true, "EXISTS": true, "TYPE": true,
	"TTL": true, "PTTL": true, "STRLEN": true, "GETRANGE": true,
	"HGET": true, "HGETALL": true, "HKEYS": true, "HVALS": true,
	"HEXISTS": true, "HLEN": true, "HMGET": true,
	"LRANGE": true, "LLEN": true, "LINDEX": true,
	"SMEMBERS": true, "SISMEMBER": true, "SCARD": true,
	"SUNION": true, "SINTER": true, "SDIFF": true,
	"ZRANGE": true, "ZRANGEBYSCORE": true, "ZSCORE": true,
	"ZCARD": true, "ZCOUNT": true, "ZRANK": true, "ZREVRANGE": true,
	"SCAN": true, "HSCAN": true, "SSCAN": true, "ZSCAN": true,
	"DBSIZE": true, "RANDOMKEY": true,
}

// isReadOnlyCommand returns true if cmd (upper-cased) is safe for replicas.
func isReadOnlyCommand(cmd string) bool {
	return readOnlyCommands[strings.ToUpper(cmd)]
}

// ClusterConfig configures a Redis Cluster client.
type ClusterConfig struct {
	// Addrs is the list of seed node addresses in "host:port" form. At least
	// one is required; the client discovers the rest via CLUSTER SLOTS.
	Addrs []string
	// Password is the AUTH credential for cluster nodes.
	Password string
	// Username is the ACL user (Redis 6+).
	Username string

	PoolSize         int
	MaxIdlePerWorker int
	DialTimeout      time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	// MaxRedirects is the maximum number of MOVED/ASK redirects before an
	// error is returned. Default: 3.
	MaxRedirects int
	// RouteByLatency, when true, sends reads to the lowest-latency node.
	// Not implemented in v1.4.0; reserved for future use.
	RouteByLatency bool
	// ReadOnly, when true, allows reads from replica nodes.
	// Not implemented in v1.4.0; reserved for future use.
	ReadOnly bool

	Engine eventloop.ServerProvider
}

// ErrClusterMaxRedirects is returned when the maximum number of MOVED/ASK
// redirects has been exhausted.
var ErrClusterMaxRedirects = errors.New("celeris/redis: cluster max redirects exceeded")

// ClusterClient is a Redis Cluster client. It maintains a slot-to-node mapping,
// routes commands by key hash slot, and handles MOVED/ASK redirects
// transparently. Each cluster node gets its own Client with its own pool.
type ClusterClient struct {
	cfg ClusterConfig

	mu       sync.RWMutex
	nodes    map[string]*clusterNode // addr -> node
	slots    [16384]*clusterNode     // slot -> primary node
	replicas [16384][]*clusterNode   // slot -> replica nodes

	replicaIdx atomic.Uint64 // round-robin counter for replica selection

	closeCh chan struct{}
	closed  atomic.Bool
}

// NewClusterClient creates a Redis Cluster client. It connects to the seed
// nodes, fetches the cluster topology via CLUSTER SLOTS, and populates the
// slot map. A background goroutine refreshes the topology periodically.
func NewClusterClient(cfg ClusterConfig) (*ClusterClient, error) {
	if len(cfg.Addrs) == 0 {
		return nil, errors.New("celeris/redis: ClusterConfig.Addrs requires at least one address")
	}
	if cfg.MaxRedirects <= 0 {
		cfg.MaxRedirects = 3
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	c := &ClusterClient{
		cfg:     cfg,
		nodes:   make(map[string]*clusterNode),
		closeCh: make(chan struct{}),
	}
	if err := c.initNodes(); err != nil {
		return nil, fmt.Errorf("celeris/redis: cluster init failed: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
	err := c.refreshTopology(ctx)
	cancel()
	if err != nil {
		c.closeNodes()
		return nil, fmt.Errorf("celeris/redis: CLUSTER SLOTS failed: %w", err)
	}
	go c.refreshLoop()
	return c, nil
}

// Close tears down all node clients and stops the background refresh.
func (c *ClusterClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.closeCh)
	c.closeNodes()
	return nil
}

// closeNodes closes all node clients.
func (c *ClusterClient) closeNodes() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.nodes {
		_ = n.Close()
	}
	c.nodes = nil
}

// initNodes dials the seed addresses and creates initial node clients.
func (c *ClusterClient) initNodes() error {
	for _, addr := range c.cfg.Addrs {
		client, err := c.dialNode(addr)
		if err != nil {
			continue
		}
		c.nodes[addr] = &clusterNode{
			addr:    addr,
			client:  client,
			primary: true,
		}
	}
	if len(c.nodes) == 0 {
		return errors.New("could not connect to any seed node")
	}
	return nil
}

// dialNode creates a single-node Client for the given address.
func (c *ClusterClient) dialNode(addr string) (*Client, error) {
	return NewClient(addr, c.nodeOpts()...)
}

// nodeOpts returns the common Option slice derived from ClusterConfig.
func (c *ClusterClient) nodeOpts() []Option {
	var opts []Option
	if c.cfg.Password != "" {
		opts = append(opts, WithPassword(c.cfg.Password))
	}
	if c.cfg.Username != "" {
		opts = append(opts, WithUsername(c.cfg.Username))
	}
	if c.cfg.DialTimeout > 0 {
		opts = append(opts, WithDialTimeout(c.cfg.DialTimeout))
	}
	if c.cfg.ReadTimeout > 0 {
		opts = append(opts, WithReadTimeout(c.cfg.ReadTimeout))
	}
	if c.cfg.WriteTimeout > 0 {
		opts = append(opts, WithWriteTimeout(c.cfg.WriteTimeout))
	}
	if c.cfg.PoolSize > 0 {
		opts = append(opts, WithPoolSize(c.cfg.PoolSize))
	}
	if c.cfg.MaxIdlePerWorker > 0 {
		opts = append(opts, WithMaxIdlePerWorker(c.cfg.MaxIdlePerWorker))
	}
	if c.cfg.Engine != nil {
		opts = append(opts, WithEngine(c.cfg.Engine))
	}
	return opts
}

// dialReplicaNode creates a Client for a replica and sends the READONLY
// command so the replica accepts read queries. If READONLY fails the client
// is closed and the error is returned.
func (c *ClusterClient) dialReplicaNode(addr string) (*Client, error) {
	client, err := NewClient(addr, c.nodeOpts()...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.DialTimeout)
	defer cancel()
	if _, err := client.Do(ctx, "READONLY"); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("celeris/redis: READONLY handshake on %s: %w", addr, err)
	}
	return client, nil
}

// getOrCreateNode returns the node for addr, creating it if absent. Nodes
// created through this path are marked as primaries.
func (c *ClusterClient) getOrCreateNode(addr string) (*clusterNode, error) {
	return c.getOrCreateNodeAs(addr, true)
}

// getOrCreateNodeAs returns the node for addr, creating it if absent. When
// isPrimary is false and ReadOnly or RouteByLatency is enabled the READONLY
// handshake is performed on the new connection.
func (c *ClusterClient) getOrCreateNodeAs(addr string, isPrimary bool) (*clusterNode, error) {
	c.mu.RLock()
	n, ok := c.nodes[addr]
	c.mu.RUnlock()
	if ok {
		return n, nil
	}
	var (
		client *Client
		err    error
	)
	if !isPrimary && (c.cfg.ReadOnly || c.cfg.RouteByLatency) {
		client, err = c.dialReplicaNode(addr)
	} else {
		client, err = c.dialNode(addr)
	}
	if err != nil {
		return nil, err
	}
	n = &clusterNode{addr: addr, client: client, primary: isPrimary}
	c.mu.Lock()
	if existing, ok := c.nodes[addr]; ok {
		c.mu.Unlock()
		_ = client.Close()
		return existing, nil
	}
	c.nodes[addr] = n
	c.mu.Unlock()
	return n, nil
}

// route returns the cluster node owning the given slot. For write commands
// (or when neither ReadOnly nor RouteByLatency is enabled), the primary is
// always returned.
func (c *ClusterClient) route(slot uint16) *clusterNode {
	c.mu.RLock()
	n := c.slots[slot]
	c.mu.RUnlock()
	return n
}

// routeForCmd picks the target node for a command+slot combination, respecting
// ReadOnly and RouteByLatency settings.
func (c *ClusterClient) routeForCmd(slot uint16, cmd string) *clusterNode {
	isRead := isReadOnlyCommand(cmd)

	c.mu.RLock()
	primary := c.slots[slot]
	reps := c.replicas[slot]
	c.mu.RUnlock()

	if !isRead || len(reps) == 0 {
		return primary
	}

	if c.cfg.RouteByLatency {
		return c.lowestLatencyNode(primary, reps)
	}

	if c.cfg.ReadOnly {
		idx := c.replicaIdx.Add(1)
		return reps[idx%uint64(len(reps))]
	}

	return primary
}

// lowestLatencyNode returns the node (primary or replica) with the lowest
// measured latency for the given slot.
func (c *ClusterClient) lowestLatencyNode(primary *clusterNode, reps []*clusterNode) *clusterNode {
	best := primary
	bestLat := primary.latency.Load()
	if bestLat == 0 {
		bestLat = math.MaxInt64
	}
	for _, r := range reps {
		lat := r.latency.Load()
		if lat == 0 {
			continue
		}
		if lat < bestLat {
			bestLat = lat
			best = r
		}
	}
	return best
}

// anyNode returns any live node, preferring non-failing nodes.
func (c *ClusterClient) anyNode() *clusterNode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, n := range c.nodes {
		if !n.failing.Load() {
			return n
		}
	}
	for _, n := range c.nodes {
		return n
	}
	return nil
}

// refreshTopology queries CLUSTER SLOTS on any live node and rebuilds the
// slot-to-node mapping, including replica nodes and per-node latency.
func (c *ClusterClient) refreshTopology(ctx context.Context) error {
	node := c.anyNode()
	if node == nil {
		return errors.New("no live nodes")
	}
	v, err := node.client.Do(ctx, "CLUSTER", "SLOTS")
	if err != nil {
		node.failing.Store(true)
		return err
	}
	node.failing.Store(false)
	if v.Type != protocol.TyArray {
		return fmt.Errorf("CLUSTER SLOTS: unexpected reply type %s", v.Type)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	wantReplicas := c.cfg.ReadOnly || c.cfg.RouteByLatency

	// Fully reset the slot→node and slot→replicas maps at the start of
	// every refresh. Previously we only reset per-range, which (a)
	// left stale replicas for slots that dropped out of the topology
	// and (b) allowed replica accumulation when two range entries
	// overlapped (resharding). A full reset costs one sweep of 16k
	// entries per refresh — negligible vs the CLUSTER SLOTS RTT.
	for s := range c.slots {
		c.slots[s] = nil
	}
	for s := range c.replicas {
		c.replicas[s] = nil
	}

	seen := make(map[string]bool)
	for _, slot := range v.Array {
		if slot.Type != protocol.TyArray || len(slot.Array) < 3 {
			continue
		}
		startSlot, _ := asInt(slot.Array[0])
		endSlot, _ := asInt(slot.Array[1])
		// slot.Array[2] is the primary: [host, port, id]
		primaryEntry := slot.Array[2]
		if primaryEntry.Type != protocol.TyArray || len(primaryEntry.Array) < 2 {
			continue
		}
		host := string(primaryEntry.Array[0].Str)
		port, _ := asInt(primaryEntry.Array[1])
		addr := net.JoinHostPort(host, strconv.FormatInt(port, 10))
		seen[addr] = true

		n, ok := c.nodes[addr]
		if !ok {
			client, err := c.dialNode(addr)
			if err != nil {
				continue
			}
			n = &clusterNode{addr: addr, client: client, primary: true}
			c.nodes[addr] = n
		}
		n.primary = true
		for s := startSlot; s <= endSlot && s < 16384; s++ {
			c.slots[s] = n
		}

		// Parse replica entries (slot.Array[3], [4], ...).
		if wantReplicas {
			for ri := 3; ri < len(slot.Array); ri++ {
				rep := slot.Array[ri]
				if rep.Type != protocol.TyArray || len(rep.Array) < 2 {
					continue
				}
				rhost := string(rep.Array[0].Str)
				rport, _ := asInt(rep.Array[1])
				raddr := net.JoinHostPort(rhost, strconv.FormatInt(rport, 10))
				seen[raddr] = true

				rn, ok := c.nodes[raddr]
				if !ok {
					client, err := c.dialReplicaNode(raddr)
					if err != nil {
						continue
					}
					rn = &clusterNode{addr: raddr, client: client, primary: false}
					c.nodes[raddr] = rn
				}
				rn.primary = false
				for s := startSlot; s <= endSlot && s < 16384; s++ {
					c.replicas[s] = append(c.replicas[s], rn)
				}
			}
		}
	}
	// Close nodes that are no longer in the topology.
	for addr, n := range c.nodes {
		if !seen[addr] {
			_ = n.Close()
			delete(c.nodes, addr)
		}
	}

	// Measure per-node latency when RouteByLatency is enabled.
	if c.cfg.RouteByLatency {
		c.measureLatencyLocked(ctx)
	}

	return nil
}

// measureLatencyLocked pings every node and records the RTT. Must be called
// with c.mu held (it reads c.nodes). The ping is best-effort — failures leave
// the previous latency value in place.
func (c *ClusterClient) measureLatencyLocked(ctx context.Context) {
	for _, n := range c.nodes {
		start := time.Now()
		if err := n.client.Ping(ctx); err != nil {
			continue
		}
		n.latency.Store(time.Since(start).Nanoseconds())
	}
}

// refreshLoop runs refreshTopology periodically (every 60s) in the
// background.
func (c *ClusterClient) refreshLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = c.refreshTopology(ctx)
			cancel()
		case <-c.closeCh:
			return
		}
	}
}

// doWithRetry routes a command by key and handles MOVED/ASK redirects. fn
// receives a context that MUST be forwarded to the Client method it calls;
// for ASK redirects the context carries a pinned connection so ASKING and the
// command share the same TCP connection.
func (c *ClusterClient) doWithRetry(ctx context.Context, key string, fn func(context.Context, *Client) error) error {
	return c.doWithRetryCmd(ctx, key, "", fn)
}

// doWithRetryCmd is like doWithRetry but accepts the command name so it can
// route read-only commands to replicas when ReadOnly or RouteByLatency is set.
// When a replica fails with a non-redirect error, the command is retried on the
// slot's primary (fallback).
func (c *ClusterClient) doWithRetryCmd(ctx context.Context, key, cmd string, fn func(context.Context, *Client) error) error {
	slot := Slot(key)
	useReplica := cmd != "" && (c.cfg.ReadOnly || c.cfg.RouteByLatency)
	var node *clusterNode
	if useReplica {
		node = c.routeForCmd(slot, cmd)
	} else {
		node = c.route(slot)
	}
	if node == nil {
		node = c.anyNode()
	}
	if node == nil {
		return errors.New("celeris/redis: no cluster nodes available")
	}
	triedReplicaFallback := false
	triedPrimaryRefresh := false
	// Loop runs up to MaxRedirects+1 times: one initial attempt
	// plus up to MaxRedirects redirects. `range N` iterates N
	// times in Go 1.22+; explicit limit + counter so the
	// off-by-one documented in the Config.MaxRedirects godoc is
	// accurate ("maximum number of MOVED/ASK redirects").
	maxAttempts := c.cfg.MaxRedirects + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	for i := 0; i < maxAttempts; i++ {
		err := fn(ctx, node.client)
		if err == nil {
			return nil
		}
		var rerr *RedisError
		if !errors.As(err, &rerr) {
			// Non-Redis error (network, etc.) on a replica: fall back to
			// the primary once before giving up.
			if !node.primary && !triedReplicaFallback {
				node.failing.Store(true)
				triedReplicaFallback = true
				if primary := c.route(slot); primary != nil {
					node = primary
					continue
				}
			}
			// Non-Redis error on a primary means the master just died
			// (crash, network partition, container stop). Without an
			// immediate topology refresh the caller would spin on the
			// dead node until the background refreshLoop fires (60s
			// ticker, first tick after the crash may also pick the dead
			// node as the "any" probe and fail its own refresh). Mark
			// the node failing, force a synchronous refresh, and retry
			// on whichever primary now owns the slot. One retry only —
			// if the refresh also fails we bubble the original error up.
			if node.primary && !triedPrimaryRefresh {
				node.failing.Store(true)
				triedPrimaryRefresh = true
				rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				refreshErr := c.refreshTopology(rctx)
				cancel()
				if refreshErr == nil {
					if newPrim := c.route(slot); newPrim != nil && newPrim != node {
						node = newPrim
						continue
					}
				}
			}
			return err
		}
		if rerr.Prefix == "MOVED" {
			addr := parseRedirectAddr(rerr.Msg)
			if addr == "" {
				return err
			}
			// Refresh topology on every MOVED — not just attempt==0.
			// A stale slot map keeps generating redirects; routing
			// by the embedded MOVED address only helps for this one
			// call, subsequent commands for the same slot still hit
			// the old node and loop.
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = c.refreshTopology(rctx)
			cancel()
			n, nerr := c.getOrCreateNode(addr)
			if nerr != nil {
				return err
			}
			node = n
			continue
		}
		if rerr.Prefix == "ASK" {
			addr := parseRedirectAddr(rerr.Msg)
			if addr == "" {
				return err
			}
			n, nerr := c.getOrCreateNode(addr)
			if nerr != nil {
				return err
			}
			return c.doASK(ctx, n.client, fn)
		}
		return err
	}
	return ErrClusterMaxRedirects
}

// doASK pins a connection, sends ASKING, then re-executes fn on the same
// conn via a context-carried pin. Redis requires ASKING and the following
// command to arrive on the same TCP connection.
func (c *ClusterClient) doASK(ctx context.Context, cl *Client, fn func(context.Context, *Client) error) error {
	conn, err := cl.pool.acquireCmd(ctx, -1)
	if err != nil {
		return err
	}
	defer cl.pool.releaseCmd(conn)
	req, err := conn.exec(ctx, "ASKING")
	if err != nil {
		return err
	}
	if req.resultErr != nil {
		conn.releaseResult(req)
		return req.resultErr
	}
	conn.releaseResult(req)
	pinCtx := context.WithValue(ctx, pinnedConnKey{}, conn)
	return fn(pinCtx, cl)
}

// parseRedirectAddr extracts the target address from a MOVED/ASK message.
// Format: "MOVED <slot> <host>:<port>" or "ASK <slot> <host>:<port>".
// The Msg field has the prefix stripped, so it's "<slot> <host>:<port>".
func parseRedirectAddr(msg string) string {
	parts := strings.Fields(msg)
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}

// ---------- Typed command methods ----------

func (c *ClusterClient) Ping(ctx context.Context) error {
	node := c.anyNode()
	if node == nil {
		return errors.New("celeris/redis: no cluster nodes available")
	}
	return node.client.Ping(ctx)
}

func (c *ClusterClient) Get(ctx context.Context, key string) (string, error) {
	var out string
	err := c.doWithRetryCmd(ctx, key, "GET", func(ctx context.Context, cl *Client) error {
		v, e := cl.Get(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) GetBytes(ctx context.Context, key string) ([]byte, error) {
	var out []byte
	err := c.doWithRetryCmd(ctx, key, "GET", func(ctx context.Context, cl *Client) error {
		v, e := cl.GetBytes(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	return c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		return cl.Set(ctx, key, value, ttl)
	})
}

func (c *ClusterClient) SetNX(ctx context.Context, key string, value any, ttl time.Duration) (bool, error) {
	var out bool
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.SetNX(ctx, key, value, ttl)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) Del(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	// Single key: route normally.
	if len(keys) == 1 {
		var out int64
		err := c.doWithRetry(ctx, keys[0], func(ctx context.Context, cl *Client) error {
			v, e := cl.Del(ctx, keys[0])
			out = v
			return e
		})
		return out, err
	}
	// Multi-key: group by slot.
	return c.multiKeyInt(ctx, "DEL", keys)
}

func (c *ClusterClient) Exists(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	if len(keys) == 1 {
		var out int64
		err := c.doWithRetryCmd(ctx, keys[0], "EXISTS", func(ctx context.Context, cl *Client) error {
			v, e := cl.Exists(ctx, keys[0])
			out = v
			return e
		})
		return out, err
	}
	return c.multiKeyInt(ctx, "EXISTS", keys)
}

// multiKeyInt handles multi-key commands (DEL, EXISTS) by grouping keys by
// slot, issuing per-node commands in parallel, and summing the results.
func (c *ClusterClient) multiKeyInt(ctx context.Context, cmd string, keys []string) (int64, error) {
	groups := make(map[uint16][]string)
	for _, k := range keys {
		slot := Slot(k)
		groups[slot] = append(groups[slot], k)
	}
	type result struct {
		n   int64
		err error
	}
	ch := make(chan result, len(groups))
	for _, group := range groups {
		group := group
		go func() {
			var out int64
			err := c.doWithRetry(ctx, group[0], func(ctx context.Context, cl *Client) error {
				v, e := cl.Do(ctx, append([]any{cmd}, anySlice(group)...)...)
				if e != nil {
					return e
				}
				n, ae := asInt(*v)
				out = n
				return ae
			})
			ch <- result{out, err}
		}()
	}
	var total int64
	var firstErr error
	for range groups {
		r := <-ch
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		total += r.n
	}
	return total, firstErr
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (c *ClusterClient) Incr(ctx context.Context, key string) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.Incr(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) Decr(ctx context.Context, key string) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.Decr(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	var out bool
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.Expire(ctx, key, expiration)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) TTL(ctx context.Context, key string) (time.Duration, error) {
	var out time.Duration
	err := c.doWithRetryCmd(ctx, key, "TTL", func(ctx context.Context, cl *Client) error {
		v, e := cl.TTL(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) HGet(ctx context.Context, key, field string) (string, error) {
	var out string
	err := c.doWithRetryCmd(ctx, key, "HGET", func(ctx context.Context, cl *Client) error {
		v, e := cl.HGet(ctx, key, field)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) HSet(ctx context.Context, key string, values ...any) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.HSet(ctx, key, values...)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.HDel(ctx, key, fields...)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	var out map[string]string
	err := c.doWithRetryCmd(ctx, key, "HGETALL", func(ctx context.Context, cl *Client) error {
		v, e := cl.HGetAll(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.LPush(ctx, key, values...)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.RPush(ctx, key, values...)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) LPop(ctx context.Context, key string) (string, error) {
	var out string
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.LPop(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) RPop(ctx context.Context, key string) (string, error) {
	var out string
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.RPop(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	var out []string
	err := c.doWithRetryCmd(ctx, key, "LRANGE", func(ctx context.Context, cl *Client) error {
		v, e := cl.LRange(ctx, key, start, stop)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.SAdd(ctx, key, members...)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) SMembers(ctx context.Context, key string) ([]string, error) {
	var out []string
	err := c.doWithRetryCmd(ctx, key, "SMEMBERS", func(ctx context.Context, cl *Client) error {
		v, e := cl.SMembers(ctx, key)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) ZAdd(ctx context.Context, key string, members ...Z) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, key, func(ctx context.Context, cl *Client) error {
		v, e := cl.ZAdd(ctx, key, members...)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) ZRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	var out []string
	err := c.doWithRetryCmd(ctx, key, "ZRANGE", func(ctx context.Context, cl *Client) error {
		v, e := cl.ZRange(ctx, key, start, stop)
		out = v
		return e
	})
	return out, err
}

func (c *ClusterClient) Publish(ctx context.Context, channel, message string) (int64, error) {
	// PUBLISH in cluster mode: route to any node (pubsub is cluster-wide).
	node := c.anyNode()
	if node == nil {
		return 0, errors.New("celeris/redis: no cluster nodes available")
	}
	return node.client.Publish(ctx, channel, message)
}

func (c *ClusterClient) Subscribe(ctx context.Context, channels ...string) (*PubSub, error) {
	// Regular SUBSCRIBE is cluster-wide: pick any node.
	node := c.anyNode()
	if node == nil {
		return nil, errors.New("celeris/redis: no cluster nodes available")
	}
	return node.client.Subscribe(ctx, channels...)
}

// SSubscribe opens a pub/sub connection and subscribes to shard channels
// (Redis 7+ SSUBSCRIBE). Shard channels are slot-scoped: the connection is
// established to the node owning the slot of the first channel. All channels
// SHOULD hash to the same slot (use hash tags); mixing slots in one PubSub is
// undefined behavior in cluster mode.
func (c *ClusterClient) SSubscribe(ctx context.Context, channels ...string) (*PubSub, error) {
	if len(channels) == 0 {
		return nil, errors.New("celeris/redis: SSubscribe requires at least one channel")
	}
	slot := Slot(channels[0])
	node := c.route(slot)
	if node == nil {
		node = c.anyNode()
	}
	if node == nil {
		return nil, errors.New("celeris/redis: no cluster nodes available")
	}
	ps, err := node.client.newPubSub(ctx)
	if err != nil {
		return nil, err
	}
	if err := ps.SSubscribe(ctx, channels...); err != nil {
		_ = ps.Close()
		return nil, err
	}
	return ps, nil
}

// SPublish publishes a message to a shard channel (Redis 7+ SPUBLISH). The
// command is routed to the node owning the channel's slot.
func (c *ClusterClient) SPublish(ctx context.Context, channel, message string) (int64, error) {
	var out int64
	err := c.doWithRetry(ctx, channel, func(ctx context.Context, cl *Client) error {
		v, e := cl.Do(ctx, "SPUBLISH", channel, message)
		if e != nil {
			return e
		}
		n, ae := asInt(*v)
		out = n
		return ae
	})
	return out, err
}

func (c *ClusterClient) Do(ctx context.Context, args ...any) (*protocol.Value, error) {
	if len(args) < 2 {
		node := c.anyNode()
		if node == nil {
			return nil, errors.New("celeris/redis: no cluster nodes available")
		}
		return node.client.Do(ctx, args...)
	}
	key := argify(args[1])
	cmd := argify(args[0])
	var out *protocol.Value
	err := c.doWithRetryCmd(ctx, key, cmd, func(ctx context.Context, cl *Client) error {
		v, e := cl.Do(ctx, args...)
		out = v
		return e
	})
	return out, err
}

// ForEachNode calls fn on every node's Client. Used for cluster-wide
// operations like FLUSHDB or DBSIZE. The ctx argument is kept for API
// symmetry; cancellation is not honored.
func (c *ClusterClient) ForEachNode(_ context.Context, fn func(*Client) error) error {
	c.mu.RLock()
	nodes := make([]*clusterNode, 0, len(c.nodes))
	for _, n := range c.nodes {
		nodes = append(nodes, n)
	}
	c.mu.RUnlock()
	var firstErr error
	for _, n := range nodes {
		if err := fn(n.client); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ---------- Cluster pipeline ----------

// ClusterPipeline groups commands by slot and executes them on the correct
// nodes in parallel. It uses per-node Pipelines internally.
type ClusterPipeline struct {
	cluster *ClusterClient
	cmds    []clusterPipeCmd
}

type clusterPipeCmd struct {
	slot    uint16
	args    []string
	kind    cmdKind
	origIdx int // populated during retry rounds
}

// Pipeline returns a ClusterPipeline that groups commands by slot.
func (c *ClusterClient) Pipeline() *ClusterPipeline {
	return &ClusterPipeline{cluster: c}
}

// Get enqueues GET.
func (cp *ClusterPipeline) Get(key string) int {
	cp.cmds = append(cp.cmds, clusterPipeCmd{
		slot: Slot(key),
		args: []string{"GET", key},
		kind: kindString,
	})
	return len(cp.cmds) - 1
}

// Set enqueues SET.
func (cp *ClusterPipeline) Set(key string, value any, ttl time.Duration) int {
	args := []string{"SET", key, argify(value)}
	args = appendExpire(args, ttl)
	cp.cmds = append(cp.cmds, clusterPipeCmd{
		slot: Slot(key),
		args: args,
		kind: kindStatus,
	})
	return len(cp.cmds) - 1
}

// Exec sends all queued commands, grouped by slot, executing per-node
// sub-pipelines in parallel. Returns per-command results and errors indexed
// by the original enqueue order. MOVED/ASK redirects within pipeline
// responses are retried once: MOVED triggers a topology refresh and re-route;
// ASK sends ASKING on the target node before re-issuing the command.
func (cp *ClusterPipeline) Exec(ctx context.Context) ([]protocol.Value, []error) {
	n := len(cp.cmds)
	if n == 0 {
		return nil, nil
	}
	// Tag origIdx for the initial round.
	tagged := make([]clusterPipeCmd, n)
	for i, cmd := range cp.cmds {
		tagged[i] = cmd
		tagged[i].origIdx = i
	}
	results := make([]protocol.Value, n)
	errs := make([]error, n)

	cp.execRound(ctx, tagged, results, errs, false)
	return results, errs
}

// stringsToAnys converts []string to []any for use with Client.Do.
func stringsToAnys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// directToValue synthesizes a protocol.Value from a pipeCmd that used the
// direct-extraction path. String bytes are copied out of the pipeline's
// slab (pc.str aliases p.strSlab which is reclaimed on Release). Scalars
// are decoded from pc.scalar according to the command kind.
func directToValue(pc *pipeCmd) protocol.Value {
	switch pc.kind {
	case kindString:
		return protocol.Value{Type: protocol.TyBulk, Str: []byte(pc.str)}
	case kindStatus:
		return protocol.Value{Type: protocol.TySimple, Str: []byte(pc.str)}
	case kindInt:
		return protocol.Value{Type: protocol.TyInt, Int: pc.scalar}
	case kindFloat:
		return protocol.Value{Type: protocol.TyDouble, Float: math.Float64frombits(uint64(pc.scalar))}
	case kindBool:
		return protocol.Value{Type: protocol.TyBool, Bool: pc.scalar != 0}
	default:
		// Unknown/array: direct path should not have been taken, but be safe.
		return protocol.Value{Type: protocol.TyBulk, Str: []byte(pc.str)}
	}
}

// execRound groups cmds by slot, runs sub-pipelines, and scans results for
// MOVED/ASK errors. On the first round (isRetry=false), redirect errors
// trigger a single retry round; on a retry round, redirect errors are
// returned as-is.
func (cp *ClusterPipeline) execRound(ctx context.Context, cmds []clusterPipeCmd, results []protocol.Value, errs []error, isRetry bool) {
	type indexedCmd struct {
		origIdx int
		cmd     clusterPipeCmd
	}
	groups := make(map[uint16][]indexedCmd)
	for _, cmd := range cmds {
		groups[cmd.slot] = append(groups[cmd.slot], indexedCmd{origIdx: cmd.origIdx, cmd: cmd})
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	for slot, icmds := range groups {
		slot, icmds := slot, icmds
		wg.Add(1)
		go func() {
			defer wg.Done()
			node := cp.cluster.route(slot)
			if node == nil {
				node = cp.cluster.anyNode()
			}
			if node == nil {
				mu.Lock()
				for _, ic := range icmds {
					errs[ic.origIdx] = errors.New("celeris/redis: no cluster nodes available")
				}
				mu.Unlock()
				return
			}
			p := node.client.Pipeline()
			defer p.Release()
			cmdIdxes := make([]int, len(icmds))
			for i, ic := range icmds {
				p.addCmd(ic.cmd.kind, ic.cmd.args...)
				cmdIdxes[i] = ic.origIdx
			}
			execErr := p.Exec(ctx)
			mu.Lock()
			for i, origIdx := range cmdIdxes {
				if execErr != nil {
					errs[origIdx] = execErr
				} else if i < len(p.cmds) {
					pc := &p.cmds[i]
					if pc.err != nil {
						errs[origIdx] = pc.err
					} else if pc.val != nil {
						results[origIdx] = *pc.val
					} else if pc.direct {
						// Direct path: typed result lives in pc.str / pc.scalar
						// and pc.str is backed by p.strSlab which will be
						// reclaimed by the deferred p.Release(). Synthesize a
						// protocol.Value and copy any string data out of the
						// slab so the caller sees stable bytes.
						results[origIdx] = directToValue(pc)
					}
				}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if isRetry {
		return
	}

	// Scan for MOVED/ASK errors and collect commands to retry.
	var retryCmds []clusterPipeCmd
	topologyRefreshed := false
	for i := range errs {
		if errs[i] == nil {
			continue
		}
		var rerr *RedisError
		if !errors.As(errs[i], &rerr) {
			continue
		}
		if rerr.Prefix == "MOVED" {
			if !topologyRefreshed {
				rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = cp.cluster.refreshTopology(rctx)
				cancel()
				topologyRefreshed = true
			}
			errs[i] = nil
			retryCmds = append(retryCmds, clusterPipeCmd{
				slot:    cp.cmds[i].slot,
				args:    cp.cmds[i].args,
				kind:    cp.cmds[i].kind,
				origIdx: i,
			})
		} else if rerr.Prefix == "ASK" {
			addr := parseRedirectAddr(rerr.Msg)
			if addr == "" {
				continue
			}
			errs[i] = nil
			n, nerr := cp.cluster.getOrCreateNode(addr)
			if nerr != nil {
				errs[i] = nerr
				continue
			}
			// Redis requires ASKING and the following command to arrive on
			// the same TCP connection. Pin a conn across both calls (mirrors
			// the single-command doASK pattern above).
			conn, aerr := n.client.pool.acquireCmd(ctx, -1)
			if aerr != nil {
				errs[i] = aerr
				continue
			}
			req, askErr := conn.exec(ctx, "ASKING")
			if askErr != nil {
				n.client.pool.releaseCmd(conn)
				errs[i] = askErr
				continue
			}
			if req.resultErr != nil {
				conn.releaseResult(req)
				n.client.pool.releaseCmd(conn)
				errs[i] = req.resultErr
				continue
			}
			conn.releaseResult(req)
			pinCtx := context.WithValue(ctx, pinnedConnKey{}, conn)
			v, doErr := n.client.Do(pinCtx, stringsToAnys(cp.cmds[i].args)...)
			n.client.pool.releaseCmd(conn)
			if doErr != nil {
				errs[i] = doErr
			} else if v != nil {
				results[i] = *v
			}
		}
	}

	if len(retryCmds) > 0 {
		cp.execRound(ctx, retryCmds, results, errs, true)
	}
}

// Discard drops all queued commands.
func (cp *ClusterPipeline) Discard() {
	cp.cmds = cp.cmds[:0]
}
