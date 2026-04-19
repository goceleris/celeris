package memcached

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// Defaults for cluster failover behavior.
const (
	defaultFailureThreshold    uint32        = 2
	defaultHealthProbeInterval time.Duration = 5 * time.Second
	probeTimeout               time.Duration = 2 * time.Second
	minProbeDelay              time.Duration = 1 * time.Second
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

	// FailureThreshold is the number of consecutive infrastructure-level
	// errors (dial failures, I/O errors, protocol corruption) that must
	// be observed on a node before it is marked as failing and skipped
	// by pickNode. Default: 2. A single transient blip must not reroute
	// traffic; two consecutive errors is the libmemcached default.
	//
	// Protocol errors (ErrCacheMiss, ErrNotStored, ErrCASConflict,
	// *MemcachedError) do NOT count toward the threshold — they are
	// server responses to valid requests.
	FailureThreshold uint32

	// HealthProbeInterval is how often a background goroutine probes
	// nodes marked as failing and clears the flag on success. Default:
	// 5 seconds. Set to 0 to disable the background probe (passive
	// healing on successful ops still applies).
	HealthProbeInterval time.Duration

	// MaxFailoverHops bounds the clockwise walk when the ring-assigned
	// node is failing. Default: len(Addrs). Successor scans are O(n)
	// in the number of nodes; this cap exists purely as a safety net
	// against pathological inputs.
	MaxFailoverHops int
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

	// failing is true when the node has exceeded FailureThreshold
	// consecutive infrastructure errors. pickNode skips failing nodes.
	failing atomic.Bool

	// lastFailAt is the UnixNano of the last failing transition. Used
	// by the probe goroutine to debounce probing immediately after a
	// failure.
	lastFailAt atomic.Int64

	// consecutiveFails counts back-to-back infra errors. Reset to zero
	// on any success.
	consecutiveFails atomic.Int32

	// index is the node's position in ClusterClient.nodes. Populated
	// once at construction and used by pickNode for O(1) lookups when
	// walking to a successor. Without it, pickNode would need to scan
	// the nodes slice to locate the failing node.
	index int
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
// boundaries (4 points per 16-byte digest). Lookup hashes the user key
// with the same MD5-first-four-little-endian form so celeris is
// drop-in compatible with libmemcached and any ketama-adjacent client
// (dalli, gomemcache/ketama, mcrouter, twemproxy): a mixed-client fleet
// all agree on which node owns which key.
type ClusterClient struct {
	cfg    ClusterConfig
	nodes  []*clusterNode // index order matches cfg.Addrs
	ring   []ringPoint    // sorted by hash; built once at construction
	closed atomic.Bool

	// probeCancel stops the background health-probe goroutine. Nil
	// when HealthProbeInterval <= 0.
	probeCancel context.CancelFunc
	// probeDone is closed by the probe goroutine on exit so Close
	// can wait for it.
	probeDone chan struct{}

	// maxHops is the realized MaxFailoverHops value (copy from cfg,
	// clamped to len(nodes) at construction).
	maxHops int
	// failureThreshold is the realized FailureThreshold (defaulted if
	// the caller passed 0).
	failureThreshold uint32
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
		return nil, errors.New("celeris-memcached: ClusterConfig.Addrs requires at least one address")
	}
	if len(cfg.Weights) != 0 && len(cfg.Weights) != len(cfg.Addrs) {
		return nil, fmt.Errorf("celeris-memcached: ClusterConfig.Weights length %d != Addrs length %d",
			len(cfg.Weights), len(cfg.Addrs))
	}
	// Cap total ring points at a safe value: 16 384 virtual nodes × 4 points
	// per digest = 65 536 points is already beyond any realistic deployment.
	// Without this, a malicious or buggy weight of math.MaxUint32 would
	// panic `make` with an OOM. Reject weights that, summed across the
	// cluster, would exceed this.
	const maxRingPoints = 1 << 16
	var totalPoints uint64
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
				return nil, fmt.Errorf("celeris-memcached: ClusterConfig.Weights[%d] is zero", i)
			}
		}
		// Guard against uint32×160 overflow into the int `total` used by
		// buildRing — on 32-bit targets this would overflow silently and
		// allocate a pathologically short slice.
		perNode := uint64(weight) * virtualNodesPerWeight
		if perNode > math.MaxInt32 || totalPoints+perNode > maxRingPoints {
			for _, n := range nodes {
				_ = n.client.Close()
			}
			return nil, fmt.Errorf("celeris-memcached: ClusterConfig.Weights[%d]=%d exceeds ring-point budget (max total %d points)",
				i, weight, maxRingPoints)
		}
		totalPoints += perNode
		client, err := NewClient(addr, nodeOpts...)
		if err != nil {
			for _, n := range nodes {
				_ = n.client.Close()
			}
			return nil, fmt.Errorf("celeris-memcached: dial %s: %w", addr, err)
		}
		nodes = append(nodes, &clusterNode{addr: addr, weight: weight, client: client, index: i})
	}
	c := &ClusterClient{cfg: cfg, nodes: nodes, ring: buildRing(nodes)}
	c.failureThreshold = cfg.FailureThreshold
	if c.failureThreshold == 0 {
		c.failureThreshold = defaultFailureThreshold
	}
	c.maxHops = cfg.MaxFailoverHops
	if c.maxHops <= 0 || c.maxHops > len(nodes) {
		c.maxHops = len(nodes)
	}

	probeInterval := cfg.HealthProbeInterval
	if probeInterval == 0 {
		probeInterval = defaultHealthProbeInterval
	}
	if probeInterval > 0 {
		pctx, cancel := context.WithCancel(context.Background())
		c.probeCancel = cancel
		c.probeDone = make(chan struct{})
		go c.probeLoop(pctx, probeInterval)
	}

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

// hashKey is the lookup hash used by pickNode. It MUST match the first
// four-byte slice of MD5(key) interpreted little-endian — the exact form
// libmemcached (and every ketama port that derives from it) uses. Using
// any other hash silently re-partitions keys differently from every
// other client on the same cluster; see the subagent review dated
// 2026-04-17 that flagged an earlier CRC32 implementation for exactly
// this incompatibility.
func hashKey(key string) uint32 {
	sum := md5.Sum([]byte(key))
	return binary.LittleEndian.Uint32(sum[0:4])
}

// pickNode returns the ring owner of key. The first ring point with
// hash >= hashKey(key) wins, wrapping to ring[0] when no such point exists.
//
// When the ring-assigned node is currently marked as failing, pickNode
// walks c.nodes clockwise from the failing node's index and returns the
// first non-failing node it encounters. The walk is bounded by
// MaxFailoverHops (default len(nodes)). If every node is failing, the
// originally assigned node is returned so the resulting error
// propagates to the caller rather than looping forever.
//
// Successor selection walks c.nodes (insertion order), NOT c.ring
// positions. The ring contains ~160 points per unit weight per node,
// so walking ring positions would cost O(virtual-nodes) hops to leave
// a failing node's range; walking c.nodes is O(len(nodes)) at most.
// This preserves the consistent-hash invariant that all keys formerly
// routed to a failing node N now route to the same successor — not
// scattered across the ring.
func (c *ClusterClient) pickNode(key string) *clusterNode {
	if len(c.ring) == 0 {
		return nil
	}
	h := hashKey(key)
	i := sort.Search(len(c.ring), func(i int) bool {
		return c.ring[i].hash >= h
	})
	if i == len(c.ring) {
		i = 0
	}
	n := c.ring[i].node
	if !n.failing.Load() {
		return n
	}
	// Walk c.nodes forward looking for a healthy neighbor.
	start := n.index
	for hop := 1; hop <= c.maxHops; hop++ {
		cand := c.nodes[(start+hop)%len(c.nodes)]
		if !cand.failing.Load() {
			return cand
		}
	}
	// Every node is failing — return the originally assigned node so
	// the caller sees the underlying error from the node's client.
	return n
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
	if c.probeCancel != nil {
		c.probeCancel()
	}
	if c.probeDone != nil {
		<-c.probeDone
	}
	var firstErr error
	for _, n := range c.nodes {
		if err := n.client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NodeStats captures the health metadata the cluster tracks per node.
// Used by diagnostic/monitoring code via [ClusterClient.NodeStats].
type NodeStats struct {
	Failing     bool
	ConsecFails int32
	LastFailAt  time.Time // zero when the node has never been marked failing
}

// NodeHealth returns a snapshot of each node's failing flag keyed by
// address. Callers hold a copy; the map is safe to mutate.
func (c *ClusterClient) NodeHealth() map[string]bool {
	out := make(map[string]bool, len(c.nodes))
	for _, n := range c.nodes {
		out[n.addr] = n.failing.Load()
	}
	return out
}

// NodeStatsMap returns a snapshot of every node's [NodeStats]. Useful
// for dashboards and readiness probes that want to distinguish "down"
// nodes from healthy ones without peeking at atomics directly.
func (c *ClusterClient) NodeStatsMap() map[string]NodeStats {
	out := make(map[string]NodeStats, len(c.nodes))
	for _, n := range c.nodes {
		ns := NodeStats{
			Failing:     n.failing.Load(),
			ConsecFails: n.consecutiveFails.Load(),
		}
		if ts := n.lastFailAt.Load(); ts > 0 {
			ns.LastFailAt = time.Unix(0, ts)
		}
		out[n.addr] = ns
	}
	return out
}

// recordResult updates the node's consecutive-failure counter and
// toggles the failing flag based on err. Called after every client
// operation dispatched through the cluster.
//
// Protocol errors (ErrCacheMiss, ErrNotStored, ErrCASConflict,
// ErrInvalidCAS, *MemcachedError) are server responses to legitimate
// requests and do NOT count as infrastructure failures.
func (n *clusterNode) recordResult(err error, threshold uint32) {
	if err == nil {
		n.consecutiveFails.Store(0)
		n.failing.Store(false)
		return
	}
	if !isInfraError(err) {
		// Reset on a "successful" protocol-level response — the server
		// answered us coherently, so connectivity is fine.
		n.consecutiveFails.Store(0)
		n.failing.Store(false)
		return
	}
	fails := n.consecutiveFails.Add(1)
	if uint32(fails) >= threshold {
		if n.failing.CompareAndSwap(false, true) {
			n.lastFailAt.Store(time.Now().UnixNano())
		}
	}
}

// isInfraError returns true for errors that indicate an infrastructure
// problem (connection/dial/I/O/protocol) rather than a protocol-level
// server response.
func isInfraError(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrCacheMiss),
		errors.Is(err, ErrNotStored),
		errors.Is(err, ErrCASConflict),
		errors.Is(err, ErrInvalidCAS),
		errors.Is(err, ErrMalformedKey),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return false
	}
	var mcErr *MemcachedError
	return !errors.As(err, &mcErr)
}

// probeLoop runs the background health probe. For each failing node
// older than minProbeDelay, it issues a lightweight Version command
// (the cheapest round trip) and clears the failing flag on success.
func (c *ClusterClient) probeLoop(ctx context.Context, interval time.Duration) {
	defer close(c.probeDone)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, n := range c.nodes {
				if !n.failing.Load() {
					continue
				}
				if time.Since(time.Unix(0, n.lastFailAt.Load())) < minProbeDelay {
					continue
				}
				pctx, cancel := context.WithTimeout(ctx, probeTimeout)
				err := n.client.Ping(pctx)
				cancel()
				if err == nil {
					n.consecutiveFails.Store(0)
					n.failing.Store(false)
				}
			}
		}
	}
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
	v, err := n.client.Get(ctx, key)
	n.recordResult(err, c.failureThreshold)
	return v, err
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
	v, err := n.client.GetBytes(ctx, key)
	n.recordResult(err, c.failureThreshold)
	return v, err
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
	err := n.client.Set(ctx, key, value, ttl)
	n.recordResult(err, c.failureThreshold)
	return err
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
	err := n.client.Add(ctx, key, value, ttl)
	n.recordResult(err, c.failureThreshold)
	return err
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
	err := n.client.Replace(ctx, key, value, ttl)
	n.recordResult(err, c.failureThreshold)
	return err
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
	err := n.client.Append(ctx, key, value)
	n.recordResult(err, c.failureThreshold)
	return err
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
	err := n.client.Prepend(ctx, key, value)
	n.recordResult(err, c.failureThreshold)
	return err
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
	ok, err := n.client.CAS(ctx, key, value, casID, ttl)
	n.recordResult(err, c.failureThreshold)
	return ok, err
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
	err := n.client.Delete(ctx, key)
	n.recordResult(err, c.failureThreshold)
	return err
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
	v, err := n.client.Incr(ctx, key, delta)
	n.recordResult(err, c.failureThreshold)
	return v, err
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
	v, err := n.client.Decr(ctx, key, delta)
	n.recordResult(err, c.failureThreshold)
	return v, err
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
	err := n.client.Touch(ctx, key, ttl)
	n.recordResult(err, c.failureThreshold)
	return err
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
	item, err := n.client.Gets(ctx, key)
	n.recordResult(err, c.failureThreshold)
	return item, err
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
	// Wrap ctx so the first sub-call that errors can cancel the rest. Without
	// this, a hung node keeps the whole GetMulti blocked until the caller's
	// own ctx deadline, even after a peer node already returned an error.
	fanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
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
			sub, err := n.client.GetMulti(fanCtx, ks...)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel() // abort the siblings
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
	fanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
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
			sub, err := n.client.GetMultiBytes(fanCtx, ks...)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
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
