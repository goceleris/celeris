package memcached

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// virtualNodesPerWeight is the number of ring points assigned to a node per
// unit of weight. Matches libmemcached's default and the broader
// ketama-compatible ecosystem (40 MD5 hashes × 4 ring points per digest).
const virtualNodesPerWeight = 160

// ClusterConfig configures a multi-server Memcached client driven by a ketama
// consistent-hash ring. Memcached servers do not gossip topology, so the
// Addrs list is authoritative and static for the lifetime of the client.
type ClusterConfig struct {
	// Addrs is the list of memcached endpoints in "host:port" form to shard
	// across. At least one address is required.
	Addrs []string

	// Weights optionally assigns a relative weight to each address, indexed
	// by position in Addrs. A nil or empty slice means equal weight. A node
	// with weight 2 owns twice as many ring points as a node with weight 1.
	// Length must either be 0 or exactly equal to len(Addrs).
	Weights []uint32

	// Protocol selects the wire dialect used by every node client. Same
	// semantics as [Config.Protocol]; default text.
	Protocol Protocol

	// MaxOpen caps the per-node connection pool. Same semantics as
	// [Config.MaxOpen]; zero means NumWorkers * 4 per node.
	MaxOpen int

	// Timeout is the advisory per-op deadline forwarded to each node client.
	Timeout time.Duration

	// DialTimeout is the TCP dial timeout forwarded to each node client.
	DialTimeout time.Duration

	// Engine hooks the cluster into a running celeris.Server's event loop.
	// If nil, a standalone loop is resolved for each node client.
	Engine eventloop.ServerProvider
}

// options materializes per-node [Option]s from the cluster config.
func (cfg ClusterConfig) options() []Option {
	opts := make([]Option, 0, 5)
	opts = append(opts, WithProtocol(cfg.Protocol))
	if cfg.MaxOpen > 0 {
		opts = append(opts, WithMaxOpen(cfg.MaxOpen))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, WithTimeout(cfg.Timeout))
	}
	if cfg.DialTimeout > 0 {
		opts = append(opts, WithDialTimeout(cfg.DialTimeout))
	}
	if cfg.Engine != nil {
		opts = append(opts, WithEngine(cfg.Engine))
	}
	return opts
}

// clusterNode is one endpoint in the ring, owning its own single-server
// [Client] and the pool behind it.
type clusterNode struct {
	addr   string
	weight uint32
	client *Client
}

// ringPoint is one slot on the ketama ring. The ring is stored sorted by hash
// ascending; lookup is a binary search with wrap-around.
type ringPoint struct {
	hash uint32
	node *clusterNode
}

// ClusterClient talks to a fixed set of memcached servers, routing each key
// through a consistent-hash ring so addition/removal of a single server only
// re-homes ~1/N of the key space. Memcached has no cluster protocol: topology
// is strictly static from the client's point of view (add a node → build a
// new ClusterClient).
//
// The ring uses the ketama algorithm: each node owns
// virtualNodesPerWeight × weight ring points, placed at MD5("addr-vnode")
// boundaries (4 points per 16-byte digest). Lookup hashes the user key with
// CRC32-IEEE — industry-standard, stdlib-only, and fast enough that the
// pick-node cost is dominated by the binary search.
type ClusterClient struct {
	cfg    ClusterConfig
	nodes  []*clusterNode // index order matches cfg.Addrs
	ring   []ringPoint    // sorted by hash; built once at construction
	closed atomic.Bool
}

// NewClusterClient builds a ring across the given memcached endpoints. Each
// address becomes its own [Client] with its own pool. If any seed address
// fails to dial the whole construction is aborted and every already-opened
// node is closed, so no goroutine or FD leaks on partial failure.
//
// Topology is static for the lifetime of the client: there is no background
// refresh loop. Memcached nodes do not gossip. To add or remove a node, tear
// the client down and build a new one.
func NewClusterClient(cfg ClusterConfig) (*ClusterClient, error) {
	if len(cfg.Addrs) == 0 {
		return nil, errors.New("celeris/memcached: ClusterConfig.Addrs requires at least one address")
	}
	if len(cfg.Weights) != 0 && len(cfg.Weights) != len(cfg.Addrs) {
		return nil, fmt.Errorf("celeris/memcached: ClusterConfig.Weights length %d != Addrs length %d",
			len(cfg.Weights), len(cfg.Addrs))
	}
	nodeOpts := cfg.options()
	nodes := make([]*clusterNode, 0, len(cfg.Addrs))
	for i, addr := range cfg.Addrs {
		weight := uint32(1)
		if len(cfg.Weights) != 0 {
			weight = cfg.Weights[i]
			if weight == 0 {
				// A zero weight would starve the node of ring points. Reject
				// it explicitly rather than silently dropping the node.
				for _, n := range nodes {
					_ = n.client.Close()
				}
				return nil, fmt.Errorf("celeris/memcached: ClusterConfig.Weights[%d] is zero", i)
			}
		}
		client, err := NewClient(addr, nodeOpts...)
		if err != nil {
			for _, n := range nodes {
				_ = n.client.Close()
			}
			return nil, fmt.Errorf("celeris/memcached: dial %s: %w", addr, err)
		}
		nodes = append(nodes, &clusterNode{addr: addr, weight: weight, client: client})
	}
	c := &ClusterClient{cfg: cfg, nodes: nodes, ring: buildRing(nodes)}
	return c, nil
}

// buildRing computes the sorted ketama ring for nodes. Each node contributes
// virtualNodesPerWeight * weight points; each MD5 digest is split into four
// 4-byte slices for four ring points. The algorithm matches libmemcached's
// ketama implementation so a client written against this ring can be swapped
// for any libmemcached-compatible counterpart without re-sharding.
func buildRing(nodes []*clusterNode) []ringPoint {
	var total int
	for _, n := range nodes {
		total += int(n.weight) * virtualNodesPerWeight
	}
	ring := make([]ringPoint, 0, total)
	for _, n := range nodes {
		// 40 MD5 digests × 4 points each = 160 points per unit weight.
		hashes := int(n.weight) * (virtualNodesPerWeight / 4)
		for i := 0; i < hashes; i++ {
			// Key format is libmemcached-compatible: "<addr>-<index>".
			key := n.addr + "-" + strconv.Itoa(i)
			sum := md5.Sum([]byte(key))
			// 16-byte digest → four uint32s, each a ring point.
			for q := 0; q < 4; q++ {
				h := binary.LittleEndian.Uint32(sum[q*4 : q*4+4])
				ring = append(ring, ringPoint{hash: h, node: n})
			}
		}
	}
	sort.Slice(ring, func(i, j int) bool { return ring[i].hash < ring[j].hash })
	return ring
}

// hashKeyCRC32 is the lookup hash. CRC32-IEEE is in the stdlib, fast, and
// uniformly distributed for the 1-250 ASCII-range keys memcached accepts. It
// is deliberately a different algorithm from the ring-build MD5: the lookup
// hash is on the hot path and favors speed, the ring-build hash is one-shot
// at construction and favors distribution quality over many short inputs.
func hashKeyCRC32(key string) uint32 {
	return crc32.ChecksumIEEE([]byte(key))
}

// pickNode returns the ring owner of key. The first ring point with
// hash >= CRC32(key) wins, wrapping to ring[0] when no such point exists.
func (c *ClusterClient) pickNode(key string) *clusterNode {
	if len(c.ring) == 0 {
		return nil
	}
	h := hashKeyCRC32(key)
	i := sort.Search(len(c.ring), func(i int) bool {
		return c.ring[i].hash >= h
	})
	if i == len(c.ring) {
		i = 0
	}
	return c.ring[i].node
}

// NodeFor returns the addr of the node that key maps to. Exposed as a stable
// API so callers can pre-compute routing, emit debug output, or shard keyed
// work outside the driver (e.g. "rebalance everything on node X before
// draining it"). Returns "" if the client has no nodes (closed or empty).
func (c *ClusterClient) NodeFor(key string) string {
	n := c.pickNode(key)
	if n == nil {
		return ""
	}
	return n.addr
}

// Addrs returns the configured address list in the original Addrs order.
// Useful for diagnostics and tests; the returned slice is a copy.
func (c *ClusterClient) Addrs() []string {
	out := make([]string, len(c.nodes))
	for i, n := range c.nodes {
		out[i] = n.addr
	}
	return out
}

// Close tears down every node's connection pool. Safe to call multiple times.
func (c *ClusterClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	var firstErr error
	for _, n := range c.nodes {
		if err := n.client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// isClosed is the standard guard at the top of every public API method.
func (c *ClusterClient) isClosed() bool { return c.closed.Load() }

// ---------- single-key routing ----------

// Get forwards to the ring owner of key.
func (c *ClusterClient) Get(ctx context.Context, key string) (string, error) {
	if c.isClosed() {
		return "", ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return "", ErrNoNodes
	}
	return n.client.Get(ctx, key)
}

// GetBytes forwards to the ring owner of key.
func (c *ClusterClient) GetBytes(ctx context.Context, key string) ([]byte, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return nil, ErrNoNodes
	}
	return n.client.GetBytes(ctx, key)
}

// Set forwards to the ring owner of key.
func (c *ClusterClient) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	if c.isClosed() {
		return ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return ErrNoNodes
	}
	return n.client.Set(ctx, key, value, ttl)
}

// Add forwards to the ring owner of key.
func (c *ClusterClient) Add(ctx context.Context, key string, value any, ttl time.Duration) error {
	if c.isClosed() {
		return ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return ErrNoNodes
	}
	return n.client.Add(ctx, key, value, ttl)
}

// Replace forwards to the ring owner of key.
func (c *ClusterClient) Replace(ctx context.Context, key string, value any, ttl time.Duration) error {
	if c.isClosed() {
		return ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return ErrNoNodes
	}
	return n.client.Replace(ctx, key, value, ttl)
}

// Append forwards to the ring owner of key.
func (c *ClusterClient) Append(ctx context.Context, key, value string) error {
	if c.isClosed() {
		return ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return ErrNoNodes
	}
	return n.client.Append(ctx, key, value)
}

// Prepend forwards to the ring owner of key.
func (c *ClusterClient) Prepend(ctx context.Context, key, value string) error {
	if c.isClosed() {
		return ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return ErrNoNodes
	}
	return n.client.Prepend(ctx, key, value)
}

// CAS forwards to the ring owner of key.
func (c *ClusterClient) CAS(ctx context.Context, key string, value any, casID uint64, ttl time.Duration) (bool, error) {
	if c.isClosed() {
		return false, ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return false, ErrNoNodes
	}
	return n.client.CAS(ctx, key, value, casID, ttl)
}

// Delete forwards to the ring owner of key.
func (c *ClusterClient) Delete(ctx context.Context, key string) error {
	if c.isClosed() {
		return ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return ErrNoNodes
	}
	return n.client.Delete(ctx, key)
}

// Incr forwards to the ring owner of key.
func (c *ClusterClient) Incr(ctx context.Context, key string, delta uint64) (uint64, error) {
	if c.isClosed() {
		return 0, ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return 0, ErrNoNodes
	}
	return n.client.Incr(ctx, key, delta)
}

// Decr forwards to the ring owner of key.
func (c *ClusterClient) Decr(ctx context.Context, key string, delta uint64) (uint64, error) {
	if c.isClosed() {
		return 0, ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return 0, ErrNoNodes
	}
	return n.client.Decr(ctx, key, delta)
}

// Touch forwards to the ring owner of key.
func (c *ClusterClient) Touch(ctx context.Context, key string, ttl time.Duration) error {
	if c.isClosed() {
		return ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return ErrNoNodes
	}
	return n.client.Touch(ctx, key, ttl)
}

// Gets forwards to the ring owner of key.
func (c *ClusterClient) Gets(ctx context.Context, key string) (CASItem, error) {
	if c.isClosed() {
		return CASItem{}, ErrClosed
	}
	n := c.pickNode(key)
	if n == nil {
		return CASItem{}, ErrNoNodes
	}
	return n.client.Gets(ctx, key)
}

// ---------- multi-key fan-out ----------

// GetMulti partitions keys by their ring owner, fans out one GetMulti per
// node in parallel, and merges the per-node results into a single map. If any
// sub-call returns an error the first such error is returned; partial
// results from the other nodes are discarded — same failure semantics as the
// single-server [Client.GetMulti].
//
// Missing keys are omitted (standard memcached semantics). The output map is
// owned by the caller.
func (c *ClusterClient) GetMulti(ctx context.Context, keys ...string) (map[string]string, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	buckets := make(map[*clusterNode][]string, len(c.nodes))
	for _, k := range keys {
		n := c.pickNode(k)
		if n == nil {
			return nil, ErrNoNodes
		}
		buckets[n] = append(buckets[n], k)
	}
	out := make(map[string]string, len(keys))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for n, ks := range buckets {
		wg.Add(1)
		go func(n *clusterNode, ks []string) {
			defer wg.Done()
			sub, err := n.client.GetMulti(ctx, ks...)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for k, v := range sub {
				out[k] = v
			}
		}(n, ks)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// GetMultiBytes is the []byte variant of [ClusterClient.GetMulti].
func (c *ClusterClient) GetMultiBytes(ctx context.Context, keys ...string) (map[string][]byte, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	if len(keys) == 0 {
		return map[string][]byte{}, nil
	}
	buckets := make(map[*clusterNode][]string, len(c.nodes))
	for _, k := range keys {
		n := c.pickNode(k)
		if n == nil {
			return nil, ErrNoNodes
		}
		buckets[n] = append(buckets[n], k)
	}
	out := make(map[string][]byte, len(keys))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for n, ks := range buckets {
		wg.Add(1)
		go func(n *clusterNode, ks []string) {
			defer wg.Done()
			sub, err := n.client.GetMultiBytes(ctx, ks...)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for k, v := range sub {
				out[k] = v
			}
		}(n, ks)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// ---------- per-server commands ----------

// Stats returns each node's statistics map, keyed by address.
func (c *ClusterClient) Stats(ctx context.Context) (map[string]map[string]string, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	out := make(map[string]map[string]string, len(c.nodes))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, n := range c.nodes {
		wg.Add(1)
		go func(n *clusterNode) {
			defer wg.Done()
			s, err := n.client.Stats(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out[n.addr] = s
		}(n)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Version returns each node's server version string, keyed by address.
func (c *ClusterClient) Version(ctx context.Context) (map[string]string, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	out := make(map[string]string, len(c.nodes))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, n := range c.nodes {
		wg.Add(1)
		go func(n *clusterNode) {
			defer wg.Done()
			v, err := n.client.Version(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out[n.addr] = v
		}(n)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Flush fans a Flush to every node in parallel. Returns the first error
// encountered; on success the whole cluster has been wiped.
func (c *ClusterClient) Flush(ctx context.Context) error {
	return c.fanout(func(n *clusterNode) error { return n.client.Flush(ctx) })
}

// FlushAfter schedules a delayed flush on every node. Each node starts its
// own local timer; the actual flushes will not be perfectly synchronized
// across nodes (memcached has no cluster-wide clock).
func (c *ClusterClient) FlushAfter(ctx context.Context, delay time.Duration) error {
	return c.fanout(func(n *clusterNode) error { return n.client.FlushAfter(ctx, delay) })
}

// Ping pings every node in parallel. Returns the first error encountered.
func (c *ClusterClient) Ping(ctx context.Context) error {
	return c.fanout(func(n *clusterNode) error { return n.client.Ping(ctx) })
}

// fanout runs fn on every node concurrently. Returns the first non-nil error.
func (c *ClusterClient) fanout(fn func(n *clusterNode) error) error {
	if c.isClosed() {
		return ErrClosed
	}
	if len(c.nodes) == 0 {
		return ErrNoNodes
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, n := range c.nodes {
		wg.Add(1)
		go func(n *clusterNode) {
			defer wg.Done()
			if err := fn(n); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	return firstErr
}
