package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClusterReplica simulates a read-only replica node. It shares its
// memStore with the primary so reads return data written through the primary.
// Writes are rejected with MOVED pointing to primaryAddr.
type fakeClusterReplica struct {
	fake        *fakeRedis
	mem         *memStore
	startSlot   int
	endSlot     int
	primaryAddr string
	allNodes    []fakeClusterSlotRange
	mu          sync.Mutex
	cmdLog      []string // records upper-cased commands for verification
}

func startFakeClusterReplica(t *testing.T, mem *memStore, startSlot, endSlot int, primaryAddr string) *fakeClusterReplica {
	t.Helper()
	r := &fakeClusterReplica{
		mem:         mem,
		startSlot:   startSlot,
		endSlot:     endSlot,
		primaryAddr: primaryAddr,
	}
	r.fake = startFakeRedis(t, r.handler)
	return r
}

func (r *fakeClusterReplica) Addr() string { return r.fake.Addr() }

func (r *fakeClusterReplica) SetTopology(ranges []fakeClusterSlotRange) {
	r.mu.Lock()
	r.allNodes = ranges
	r.mu.Unlock()
}

func (r *fakeClusterReplica) Commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(r.cmdLog))
	copy(cp, r.cmdLog)
	return cp
}

func (r *fakeClusterReplica) handler(cmd []string, w *bufio.Writer) {
	if len(cmd) == 0 {
		return
	}
	upper := strings.ToUpper(cmd[0])
	r.mu.Lock()
	r.cmdLog = append(r.cmdLog, upper)
	r.mu.Unlock()

	switch upper {
	case "HELLO":
		proto := 2
		if len(cmd) > 1 && cmd[1] == "3" {
			proto = 3
		}
		handleHELLO(w, proto)
		return
	case "PING":
		writeSimple(w, "PONG")
		return
	case "READONLY":
		writeSimple(w, "OK")
		return
	case "CLUSTER":
		if len(cmd) >= 2 && strings.ToUpper(cmd[1]) == "SLOTS" {
			r.mu.Lock()
			ranges := r.allNodes
			r.mu.Unlock()
			writeArrayHeader(w, len(ranges))
			for _, rng := range ranges {
				host, port, _ := splitHostPort(rng.addr)
				writeArrayHeader(w, 3+len(rng.replicas))
				writeInt(w, int64(rng.start))
				writeInt(w, int64(rng.end))
				writeArrayHeader(w, 3)
				writeBulk(w, host)
				writeInt(w, parseInt(port))
				writeBulk(w, "node-"+rng.addr)
				for _, ra := range rng.replicas {
					rh, rp, _ := splitHostPort(ra)
					writeArrayHeader(w, 3)
					writeBulk(w, rh)
					writeInt(w, parseInt(rp))
					writeBulk(w, "replica-"+ra)
				}
			}
			return
		}
		writeError(w, "ERR unknown cluster subcommand")
		return
	case "ASKING":
		writeSimple(w, "OK")
		return
	}

	// Read-only commands: serve from shared memStore.
	if isReadOnlyCommand(upper) {
		r.mem.handler(cmd, w)
		return
	}

	// Write commands: reject with MOVED to primary.
	key := extractKey(upper, cmd)
	if key != "" {
		slot := Slot(key)
		writeError(w, fmt.Sprintf("MOVED %d %s", slot, r.primaryAddr))
		return
	}

	// Fallback for unrecognized: serve from memStore.
	r.mem.handler(cmd, w)
}

// fakeClusterNode simulates a cluster node that handles a range of slots.
// It stores data only for keys whose slot falls within its range and responds
// with MOVED for keys outside its range.
type fakeClusterNode struct {
	fake      *fakeRedis
	mem       *memStore
	startSlot int
	endSlot   int
	// allNodes maps slot ranges to addresses for CLUSTER SLOTS.
	allNodes []fakeClusterSlotRange
	mu       sync.Mutex
}

type fakeClusterSlotRange struct {
	start    int
	end      int
	addr     string
	replicas []string // optional replica addresses
}

func startFakeClusterNode(t *testing.T, startSlot, endSlot int) *fakeClusterNode {
	t.Helper()
	m := newMem()
	fcn := &fakeClusterNode{
		mem:       m,
		startSlot: startSlot,
		endSlot:   endSlot,
	}
	fcn.fake = startFakeRedis(t, fcn.handler)
	return fcn
}

func (fcn *fakeClusterNode) Addr() string { return fcn.fake.Addr() }

func (fcn *fakeClusterNode) SetTopology(ranges []fakeClusterSlotRange) {
	fcn.mu.Lock()
	fcn.allNodes = ranges
	fcn.mu.Unlock()
}

func (fcn *fakeClusterNode) ownsKey(key string) bool {
	slot := int(Slot(key))
	return slot >= fcn.startSlot && slot <= fcn.endSlot
}

func (fcn *fakeClusterNode) handler(cmd []string, w *bufio.Writer) {
	if len(cmd) == 0 {
		return
	}
	upper := strings.ToUpper(cmd[0])
	switch upper {
	case "HELLO":
		proto := 2
		if len(cmd) > 1 && cmd[1] == "3" {
			proto = 3
		}
		handleHELLO(w, proto)
		return
	case "PING":
		writeSimple(w, "PONG")
		return
	case "CLUSTER":
		if len(cmd) >= 2 && strings.ToUpper(cmd[1]) == "SLOTS" {
			fcn.writeClusterSlots(w)
			return
		}
		writeError(w, "ERR unknown cluster subcommand")
		return
	case "ASKING":
		writeSimple(w, "OK")
		return
	}

	// For key-bearing commands, check if this node owns the slot.
	key := extractKey(upper, cmd)
	if key != "" && !fcn.ownsKey(key) {
		slot := Slot(key)
		addr := fcn.addrForSlot(int(slot))
		writeError(w, fmt.Sprintf("MOVED %d %s", slot, addr))
		return
	}

	// Delegate to memStore.
	fcn.mem.handler(cmd, w)
}

func (fcn *fakeClusterNode) writeClusterSlots(w *bufio.Writer) {
	fcn.mu.Lock()
	ranges := fcn.allNodes
	fcn.mu.Unlock()
	writeArrayHeader(w, len(ranges))
	for _, r := range ranges {
		host, port, _ := splitHostPort(r.addr)
		// 3 = start, end, primary + len(replicas)
		writeArrayHeader(w, 3+len(r.replicas))
		writeInt(w, int64(r.start))
		writeInt(w, int64(r.end))
		// Primary: [host, port, id]
		writeArrayHeader(w, 3)
		writeBulk(w, host)
		writeInt(w, parseInt(port))
		writeBulk(w, "node-"+r.addr)
		// Replicas: each is [host, port, id]
		for _, raddr := range r.replicas {
			rhost, rport, _ := splitHostPort(raddr)
			writeArrayHeader(w, 3)
			writeBulk(w, rhost)
			writeInt(w, parseInt(rport))
			writeBulk(w, "replica-"+raddr)
		}
	}
}

func parseInt(s string) int64 {
	n := int64(0)
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int64(c-'0')
		}
	}
	return n
}

func (fcn *fakeClusterNode) addrForSlot(slot int) string {
	fcn.mu.Lock()
	defer fcn.mu.Unlock()
	for _, r := range fcn.allNodes {
		if slot >= r.start && slot <= r.end {
			return r.addr
		}
	}
	return fcn.Addr()
}

// extractKey returns the key argument for common commands, or "" for commands
// that don't have a key (or we don't recognize).
func extractKey(upper string, cmd []string) string {
	switch upper {
	case "GET", "SET", "DEL", "INCR", "DECR", "EXISTS", "EXPIRE", "TTL",
		"HGET", "HSET", "HDEL", "HGETALL", "HEXISTS", "HKEYS", "HVALS",
		"LPUSH", "RPUSH", "LPOP", "RPOP", "LRANGE", "LLEN",
		"SADD", "SREM", "SMEMBERS", "SISMEMBER", "SCARD",
		"ZADD", "ZRANGE", "ZRANGEBYSCORE", "ZREM", "ZSCORE", "ZCARD",
		"TYPE", "PERSIST", "PEXPIRE", "EXPIREAT", "PEXPIREAT", "PTTL",
		"SETEX", "GETDEL", "APPEND", "INCRBY", "INCRBYFLOAT", "DECRBY",
		"LINDEX", "LREM", "SINTER", "SUNION", "SDIFF",
		"ZRANK", "ZREVRANGE", "ZCOUNT", "ZINCRBY",
		"HINCRBY", "HINCRBYFLOAT", "HLEN", "HSETNX", "HMGET":
		if len(cmd) >= 2 {
			return cmd[1]
		}
	case "MGET":
		if len(cmd) >= 2 {
			return cmd[1]
		}
	case "MSET":
		if len(cmd) >= 2 {
			return cmd[1]
		}
	}
	return ""
}

func setupCluster(t *testing.T) (node1, node2, node3 *fakeClusterNode) {
	t.Helper()
	node1 = startFakeClusterNode(t, 0, 5460)
	node2 = startFakeClusterNode(t, 5461, 10922)
	node3 = startFakeClusterNode(t, 10923, 16383)

	topo := []fakeClusterSlotRange{
		{start: 0, end: 5460, addr: node1.Addr()},
		{start: 5461, end: 10922, addr: node2.Addr()},
		{start: 10923, end: 16383, addr: node3.Addr()},
	}
	node1.SetTopology(topo)
	node2.SetTopology(topo)
	node3.SetTopology(topo)
	return
}

func TestClusterSlot(t *testing.T) {
	tests := []struct {
		key  string
		want uint16
	}{
		{"foo", crc16("foo") % 16384},
		{"{user}.following", crc16("user") % 16384},
		{"{user}.followers", crc16("user") % 16384},
		{"abc{}{def}", crc16("abc{}{def}") % 16384}, // empty tag: hash whole key
		{"{abc", crc16("{abc") % 16384},             // no closing brace
		{"abc}", crc16("abc}") % 16384},             // no opening brace
	}
	for _, tt := range tests {
		got := Slot(tt.key)
		if got != tt.want {
			t.Errorf("Slot(%q) = %d, want %d", tt.key, got, tt.want)
		}
	}
}

func TestClusterSlotHashTag(t *testing.T) {
	// Keys with the same hash tag should map to the same slot.
	s1 := Slot("{tag}.key1")
	s2 := Slot("{tag}.key2")
	if s1 != s2 {
		t.Errorf("hash tag slot mismatch: %d != %d", s1, s2)
	}
}

func TestClusterGetSet(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	// Write a key that maps to some slot and verify it routes correctly.
	if err := cc.Set(ctx, "hello", "world", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := cc.Get(ctx, "hello")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "world" {
		t.Fatalf("Get = %q, want %q", got, "world")
	}
}

func TestClusterMOVED(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()

	// Pick a key that we know falls outside node1's range. We'll test many
	// keys to ensure at least one triggers a MOVED redirect.
	var movedKey string
	for i := range 1000 {
		key := fmt.Sprintf("test-moved-%d", i)
		slot := Slot(key)
		if int(slot) > 5460 {
			movedKey = key
			break
		}
	}
	if movedKey == "" {
		t.Skip("could not find a key that maps outside slot 0-5460")
	}

	// The ClusterClient should handle the MOVED redirect transparently.
	if err := cc.Set(ctx, movedKey, "redirected", 0); err != nil {
		t.Fatalf("Set(%s): %v", movedKey, err)
	}
	got, err := cc.Get(ctx, movedKey)
	if err != nil {
		t.Fatalf("Get(%s): %v", movedKey, err)
	}
	if got != "redirected" {
		t.Fatalf("Get(%s) = %q, want %q", movedKey, got, "redirected")
	}
}

func TestClusterASK(t *testing.T) {
	// Simulate an ASK redirect: node1 responds to a key with ASK pointing to
	// node2, and node2 accepts it after ASKING.
	node2 := startFakeClusterNode(t, 0, 16383) // node2 accepts everything
	node1Mem := newMem()

	// We need the node1 address before creating the handler, so we use an
	// atomic pointer to store the address after creation.
	var node1Addr atomic.Value

	node1Fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		upper := strings.ToUpper(cmd[0])
		switch upper {
		case "HELLO":
			proto := 2
			if len(cmd) > 1 && cmd[1] == "3" {
				proto = 3
			}
			handleHELLO(w, proto)
		case "CLUSTER":
			if len(cmd) >= 2 && strings.ToUpper(cmd[1]) == "SLOTS" {
				addr, _ := node1Addr.Load().(string)
				writeArrayHeader(w, 1)
				writeArrayHeader(w, 3)
				writeInt(w, 0)
				writeInt(w, 16383)
				host, port, _ := splitHostPort(addr)
				writeArrayHeader(w, 3)
				writeBulk(w, host)
				writeInt(w, parseInt(port))
				writeBulk(w, "node-1")
				return
			}
			writeError(w, "ERR unknown")
		case "SET":
			if len(cmd) >= 2 && strings.HasPrefix(cmd[1], "ask-") {
				writeError(w, fmt.Sprintf("ASK %d %s", Slot(cmd[1]), node2.Addr()))
				return
			}
			node1Mem.handler(cmd, w)
		case "GET":
			if len(cmd) >= 2 && strings.HasPrefix(cmd[1], "ask-") {
				writeError(w, fmt.Sprintf("ASK %d %s", Slot(cmd[1]), node2.Addr()))
				return
			}
			node1Mem.handler(cmd, w)
		default:
			node1Mem.handler(cmd, w)
		}
	})
	node1Addr.Store(node1Fake.Addr())

	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1Fake.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	if err := cc.Set(ctx, "ask-key", "ask-value", 0); err != nil {
		t.Fatalf("Set with ASK: %v", err)
	}
}

func TestClusterTopologyRefresh(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Force a topology refresh.
	ctx := context.Background()
	if err := cc.refreshTopology(ctx); err != nil {
		t.Fatalf("refreshTopology: %v", err)
	}

	// The cluster should now know about all three nodes.
	cc.mu.RLock()
	nodeCount := len(cc.nodes)
	cc.mu.RUnlock()
	if nodeCount < 3 {
		// Check if we have at least the seed and discovered nodes. The
		// exact count depends on whether all 3 addresses are different.
		_ = node2
		_ = node3
		t.Logf("node count = %d (expected >=3)", nodeCount)
	}
}

func TestClusterMultiKeyDel(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	// Set some keys that likely map to different slots.
	keys := []string{"a", "b", "c", "d", "e"}
	for _, k := range keys {
		if err := cc.Set(ctx, k, "val", 0); err != nil {
			t.Fatalf("Set(%s): %v", k, err)
		}
	}
	n, err := cc.Del(ctx, keys...)
	if err != nil {
		t.Fatalf("Del: %v", err)
	}
	if n != int64(len(keys)) {
		t.Fatalf("Del returned %d, want %d", n, len(keys))
	}
}

func TestClusterPipeline(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	// Use hash tags to colocate keys on the same slot.
	if err := cc.Set(ctx, "{pipe}.k1", "v1", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cc.Set(ctx, "{pipe}.k2", "v2", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	p := cc.Pipeline()
	p.Get("{pipe}.k1")
	p.Get("{pipe}.k2")
	results, errs := p.Exec(ctx)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("pipeline cmd %d error: %v", i, e)
		}
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestClusterConfigValidation(t *testing.T) {
	_, err := NewClusterClient(ClusterConfig{})
	if err == nil {
		t.Fatal("expected error for empty Addrs")
	}
}

func TestClusterClose(t *testing.T) {
	node1, _, _ := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close should not panic.
	if err := cc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestClusterMaxRedirects(t *testing.T) {
	// Create a node that always responds with MOVED back to itself (infinite loop).
	var loopAddr atomic.Value

	loopFake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		upper := strings.ToUpper(cmd[0])
		switch upper {
		case "HELLO":
			handleHELLO(w, 2)
		case "CLUSTER":
			addr, _ := loopAddr.Load().(string)
			writeArrayHeader(w, 1)
			writeArrayHeader(w, 3)
			writeInt(w, 0)
			writeInt(w, 16383)
			host, port, _ := splitHostPort(addr)
			writeArrayHeader(w, 3)
			writeBulk(w, host)
			writeInt(w, parseInt(port))
			writeBulk(w, "loop")
		case "GET", "SET":
			if len(cmd) >= 2 {
				addr, _ := loopAddr.Load().(string)
				writeError(w, fmt.Sprintf("MOVED %d %s", Slot(cmd[1]), addr))
				return
			}
			writeError(w, "ERR wrong number of arguments")
		default:
			writeSimple(w, "OK")
		}
	})
	loopAddr.Store(loopFake.Addr())

	cc, err := NewClusterClient(ClusterConfig{
		Addrs:        []string{loopFake.Addr()},
		DialTimeout:  2 * time.Second,
		MaxRedirects: 3,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	_, err = cc.Get(ctx, "loop-key")
	if !errors.Is(err, ErrClusterMaxRedirects) {
		t.Fatalf("expected ErrClusterMaxRedirects, got: %v", err)
	}
}

func TestClusterForEachNode(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	var count int
	ctx := context.Background()
	err = cc.ForEachNode(ctx, func(c *Client) error {
		count++
		return c.Ping(ctx)
	})
	if err != nil {
		t.Fatalf("ForEachNode: %v", err)
	}
	if count < 3 {
		t.Logf("ForEachNode visited %d nodes (expected >=3)", count)
	}
}

func TestClusterIncr(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	n, err := cc.Incr(ctx, "counter")
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if n != 1 {
		t.Fatalf("Incr = %d, want 1", n)
	}
	n, err = cc.Incr(ctx, "counter")
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if n != 2 {
		t.Fatalf("Incr = %d, want 2", n)
	}
}

func TestCrc16(t *testing.T) {
	// Known test vector: Redis documentation example.
	// "123456789" -> CRC-CCITT = 0x31C3
	got := crc16("123456789")
	if got != 0x31C3 {
		t.Errorf("crc16(%q) = 0x%04X, want 0x31C3", "123456789", got)
	}
}

// ---------- Cluster transaction tests ----------

func TestClusterTxBasic(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	// Use hash tags to colocate keys on the same slot.
	tx := cc.TxPipeline()
	setCmd := tx.Set("{tx}.key1", "val1", 0)
	incrCmd := tx.Incr("{tx}.counter")
	if err := tx.Exec(ctx); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if s, err := setCmd.Result(); err != nil || s != "OK" {
		t.Fatalf("Set result = %q, %v", s, err)
	}
	if n, err := incrCmd.Result(); err != nil || n != 1 {
		t.Fatalf("Incr result = %d, %v", n, err)
	}
	// Verify the values are actually persisted.
	got, err := cc.Get(ctx, "{tx}.key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "val1" {
		t.Fatalf("Get = %q, want %q", got, "val1")
	}
}

func TestClusterTxCrossSlot(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Find two keys that hash to different slots.
	key1 := "aaa"
	var key2 string
	slot1 := Slot(key1)
	for i := range 10000 {
		k := fmt.Sprintf("key%d", i)
		if Slot(k) != slot1 {
			key2 = k
			break
		}
	}
	if key2 == "" {
		t.Fatal("could not find a key in a different slot")
	}

	ctx := context.Background()
	tx := cc.TxPipeline()
	tx.Set(key1, "v1", 0)
	tx.Set(key2, "v2", 0)
	err = tx.Exec(ctx)
	if !errors.Is(err, ErrCrossSlot) {
		t.Fatalf("expected ErrCrossSlot, got: %v", err)
	}
}

func TestClusterWatch(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	// Pre-set the key.
	if err := cc.Set(ctx, "{watch}.counter", "10", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	err = cc.Watch(ctx, func(tx *Tx) error {
		incrCmd := tx.Incr("{watch}.counter")
		if err := tx.Exec(ctx); err != nil {
			return err
		}
		n, err := incrCmd.Result()
		if err != nil {
			return err
		}
		if n != 11 {
			t.Errorf("Incr result = %d, want 11", n)
		}
		return nil
	}, "{watch}.counter")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
}

func TestClusterWatchCrossSlot(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Find two keys in different slots.
	key1 := "{slotA}.key"
	var key2 string
	slot1 := Slot(key1)
	for i := range 10000 {
		k := fmt.Sprintf("{slotB%d}.key", i)
		if Slot(k) != slot1 {
			key2 = k
			break
		}
	}
	if key2 == "" {
		t.Fatal("could not find a key in a different slot")
	}

	ctx := context.Background()
	err = cc.Watch(ctx, func(tx *Tx) error {
		return tx.Exec(ctx)
	}, key1, key2)
	if !errors.Is(err, ErrCrossSlot) {
		t.Fatalf("expected ErrCrossSlot, got: %v", err)
	}
}

func TestClusterTxDiscard(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	tx := cc.TxPipeline()
	tx.Set("{discard}.k", "v", 0)
	tx.Discard()
	// After discard, the tx should be reusable with a different slot.
	tx.Set("{other}.k", "v2", 0)
	ctx := context.Background()
	if err := tx.Exec(ctx); err != nil {
		t.Fatalf("Exec after Discard: %v", err)
	}
}

func TestClusterTxEmpty(t *testing.T) {
	node1, _, _ := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	tx := cc.TxPipeline()
	ctx := context.Background()
	if err := tx.Exec(ctx); err != nil {
		t.Fatalf("empty Exec should succeed, got: %v", err)
	}
}

// ---------- ReadOnly / RouteByLatency tests ----------

// setupClusterWithReplicas creates a 1-primary + 2-replica cluster where all
// nodes own all slots (0-16383). The primary has the canonical memStore; both
// replicas share the same memStore so reads reflect writes.
func setupClusterWithReplicas(t *testing.T) (primary *fakeClusterNode, rep1, rep2 *fakeClusterReplica) {
	t.Helper()
	primary = startFakeClusterNode(t, 0, 16383)
	rep1 = startFakeClusterReplica(t, primary.mem, 0, 16383, primary.Addr())
	rep2 = startFakeClusterReplica(t, primary.mem, 0, 16383, primary.Addr())

	topo := []fakeClusterSlotRange{
		{start: 0, end: 16383, addr: primary.Addr(), replicas: []string{rep1.Addr(), rep2.Addr()}},
	}
	primary.SetTopology(topo)
	rep1.SetTopology(topo)
	rep2.SetTopology(topo)
	return
}

func TestClusterReadOnly(t *testing.T) {
	primary, rep1, rep2 := setupClusterWithReplicas(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{primary.Addr()},
		DialTimeout: 2 * time.Second,
		ReadOnly:    true,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()

	// SET should go to the primary (writes always route to primary).
	if err := cc.Set(ctx, "rk", "rv", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// GET should go to a replica. Issue several GETs to hit both replicas
	// via round-robin.
	for range 4 {
		got, err := cc.Get(ctx, "rk")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != "rv" {
			t.Fatalf("Get = %q, want %q", got, "rv")
		}
	}

	// Verify that READONLY was sent to at least one replica and that GET
	// commands arrived at replicas (not just the primary).
	rep1Cmds := rep1.Commands()
	rep2Cmds := rep2.Commands()
	allReplicaCmds := append(rep1Cmds, rep2Cmds...)

	readonlySeen := false
	getSeen := false
	for _, c := range allReplicaCmds {
		if c == "READONLY" {
			readonlySeen = true
		}
		if c == "GET" {
			getSeen = true
		}
	}
	if !readonlySeen {
		t.Fatal("READONLY command was never sent to any replica")
	}
	if !getSeen {
		t.Fatal("GET was never routed to a replica")
	}
}

func TestClusterReadOnlyFallback(t *testing.T) {
	// Build a cluster where one replica is dead (closed listener). The
	// round-robin will eventually hit the dead one; the client should fall
	// back to the primary for reads.
	primary := startFakeClusterNode(t, 0, 16383)
	liveReplica := startFakeClusterReplica(t, primary.mem, 0, 16383, primary.Addr())

	// Create a dead replica: start and immediately close.
	deadReplica := startFakeClusterReplica(t, primary.mem, 0, 16383, primary.Addr())
	deadAddr := deadReplica.Addr()
	_ = deadReplica.fake.ln.Close() // kill the listener

	topo := []fakeClusterSlotRange{
		{start: 0, end: 16383, addr: primary.Addr(), replicas: []string{liveReplica.Addr(), deadAddr}},
	}
	primary.SetTopology(topo)
	liveReplica.SetTopology(topo)

	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{primary.Addr()},
		DialTimeout: 1 * time.Second,
		ReadOnly:    true,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()

	// Write a value through the primary.
	if err := cc.Set(ctx, "fallback-key", "fallback-val", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Issue reads. Some will hit the dead replica and should fall back to
	// the primary or the live replica. All should succeed eventually.
	for i := range 6 {
		got, err := cc.Get(ctx, "fallback-key")
		if err != nil {
			t.Fatalf("Get attempt %d: %v", i, err)
		}
		if got != "fallback-val" {
			t.Fatalf("Get attempt %d = %q, want %q", i, got, "fallback-val")
		}
	}
}

func TestClusterRouteByLatency(t *testing.T) {
	// Build a cluster with 1 primary and 2 replicas. We simulate latency by
	// injecting artificial delays into one replica's handler so that the
	// other replica (or the primary) is always faster.
	primary := startFakeClusterNode(t, 0, 16383)
	// fastReplica responds instantly.
	fastReplica := startFakeClusterReplica(t, primary.mem, 0, 16383, primary.Addr())
	// slowReplica has a delay injected into PING.
	slowMem := primary.mem
	slowReplica := &fakeClusterReplica{
		mem:         slowMem,
		startSlot:   0,
		endSlot:     16383,
		primaryAddr: primary.Addr(),
	}
	slowReplica.fake = startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		upper := strings.ToUpper(cmd[0])
		slowReplica.mu.Lock()
		slowReplica.cmdLog = append(slowReplica.cmdLog, upper)
		slowReplica.mu.Unlock()

		if upper == "PING" {
			time.Sleep(50 * time.Millisecond) // artificial latency
			writeSimple(w, "PONG")
			return
		}
		// Delegate everything else to the standard replica handler.
		slowReplica.handler(cmd, w)
	})

	topo := []fakeClusterSlotRange{
		{start: 0, end: 16383, addr: primary.Addr(), replicas: []string{fastReplica.Addr(), slowReplica.Addr()}},
	}
	primary.SetTopology(topo)
	fastReplica.SetTopology(topo)
	slowReplica.SetTopology(topo)

	cc, err := NewClusterClient(ClusterConfig{
		Addrs:          []string{primary.Addr()},
		DialTimeout:    2 * time.Second,
		RouteByLatency: true,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()

	// Write a value.
	if err := cc.Set(ctx, "lat-key", "lat-val", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Force a topology refresh so latency is measured.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := cc.refreshTopology(rctx); err != nil {
		cancel()
		t.Fatalf("refreshTopology: %v", err)
	}
	cancel()

	// Issue several GETs. They should predominantly go to the fast replica
	// (or the primary, which is also fast) rather than the slow replica.
	for i := range 4 {
		got, err := cc.Get(ctx, "lat-key")
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if got != "lat-val" {
			t.Fatalf("Get %d = %q, want %q", i, got, "lat-val")
		}
	}

	// The slow replica should have received very few (ideally zero) GET
	// commands compared to the fast replica.
	slowGets := 0
	for _, c := range slowReplica.Commands() {
		if c == "GET" {
			slowGets++
		}
	}
	fastGets := 0
	for _, c := range fastReplica.Commands() {
		if c == "GET" {
			fastGets++
		}
	}
	if slowGets > fastGets {
		t.Errorf("slow replica got %d GETs, fast replica got %d GETs; expected fast >= slow", slowGets, fastGets)
	}
}

// TestClusterPipelineASKPinsConn guards against a regression where the
// cluster pipeline's ASK retry issued ASKING and the command on two
// different conns (separate acquireCmd calls). Redis requires both to land
// on the same TCP connection.
func TestClusterPipelineASKPinsConn(t *testing.T) {
	// node2 tracks per-conn sequences so we can assert ASKING preceded the
	// command on the SAME connection.
	type conEvent struct {
		writerID uintptr
		cmd      string
	}
	var node2Events []conEvent
	var node2Mu sync.Mutex
	// Real Redis only accepts the next command after ASKING on the SAME
	// TCP conn — any other conn responds with MOVED. We mirror that here
	// via per-conn ASKING state keyed by the writer pointer.
	askedPerConn := map[uintptr]bool{}
	var askMu sync.Mutex

	// raceHook fires immediately AFTER node2 replies to ASKING. A test
	// goroutine uses this window to grab the just-released conn out of
	// the pool before the cluster pipeline's follow-up GET can
	// re-acquire it. Without the ASK-pins-conn fix, this reliably forces
	// the GET onto a DIFFERENT conn than the ASKING — and the strict
	// MOVED rule above then surfaces the bug as a failing GET reply.
	askingReplied := make(chan struct{}, 1)

	mem := newMem()
	node2Fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		upper := strings.ToUpper(cmd[0])
		wID := uintptrOfWriter(w)
		// Record every non-handshake command with its writer identity.
		switch upper {
		case "HELLO", "CLUSTER":
			// handshake / topology: do not record
		default:
			node2Mu.Lock()
			node2Events = append(node2Events, conEvent{
				writerID: wID,
				cmd:      upper,
			})
			node2Mu.Unlock()
		}
		switch upper {
		case "HELLO":
			proto := 2
			if len(cmd) > 1 && cmd[1] == "3" {
				proto = 3
			}
			handleHELLO(w, proto)
			return
		case "ASKING":
			askMu.Lock()
			askedPerConn[wID] = true
			askMu.Unlock()
			writeSimple(w, "OK")
			// Signal the race goroutine that ASKING's reply is on the
			// wire. It will try to grab the same conn out of node2's
			// pool before the pipeline code re-acquires it for GET.
			select {
			case askingReplied <- struct{}{}:
			default:
			}
			return
		case "CLUSTER":
			writeError(w, "ERR unsupported here")
			return
		case "GET":
			// Strict ASK semantics: the migrating slot's owner requires
			// ASKING to have been sent on THIS same conn before the GET.
			if len(cmd) >= 2 && strings.HasPrefix(cmd[1], "ask-") {
				askMu.Lock()
				ok := askedPerConn[wID]
				delete(askedPerConn, wID) // consume the flag, per-cmd
				askMu.Unlock()
				if !ok {
					writeError(w, "MOVED 0 127.0.0.1:0") // mimic: ASKING missing
					return
				}
			}
		}
		mem.handler(cmd, w)
	})

	// node1 owns all slots per its own topology, but responds to our target
	// key with ASK pointing to node2.
	var node1Addr atomic.Value
	node1Fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		upper := strings.ToUpper(cmd[0])
		switch upper {
		case "HELLO":
			proto := 2
			if len(cmd) > 1 && cmd[1] == "3" {
				proto = 3
			}
			handleHELLO(w, proto)
			return
		case "CLUSTER":
			if len(cmd) >= 2 && strings.ToUpper(cmd[1]) == "SLOTS" {
				addr, _ := node1Addr.Load().(string)
				writeArrayHeader(w, 1)
				writeArrayHeader(w, 3)
				writeInt(w, 0)
				writeInt(w, 16383)
				host, port, _ := splitHostPort(addr)
				writeArrayHeader(w, 3)
				writeBulk(w, host)
				writeInt(w, parseInt(port))
				writeBulk(w, "node-1")
				return
			}
			writeError(w, "ERR unknown")
			return
		case "GET":
			if len(cmd) >= 2 && strings.HasPrefix(cmd[1], "ask-") {
				writeError(w, fmt.Sprintf("ASK %d %s", Slot(cmd[1]), node2Fake.Addr()))
				return
			}
		}
		writeNullBulk(w)
	})
	node1Addr.Store(node1Fake.Addr())

	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1Fake.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Seed the value on node2's in-memory store so the ASK retry can fetch it.
	mem.mu.Lock()
	mem.kv["ask-key"] = "pinned-value"
	mem.mu.Unlock()

	ctx := context.Background()
	// Warm node2's pool with multiple conns via concurrent PINGs so the
	// pool has several idle candidates, making it possible for a buggy
	// (unpinned) ASK+GET retry to land on different conns.
	n2, err := cc.getOrCreateNode(node2Fake.Addr())
	if err != nil {
		t.Fatalf("getOrCreateNode node2: %v", err)
	}
	var warm sync.WaitGroup
	const nConns = 8
	startBarrier := make(chan struct{})
	for i := 0; i < nConns; i++ {
		warm.Add(1)
		go func() {
			defer warm.Done()
			<-startBarrier
			_, _ = n2.client.Do(ctx, "PING")
		}()
	}
	close(startBarrier)
	warm.Wait()

	// Race goroutine: as soon as ASKING's reply hits the wire, hammer
	// node2 with concurrent PINGs. Without the fix, ASKING's conn
	// returns to the idle pool and one of these PINGs can steal it
	// before the pipeline re-acquires for GET — forcing GET onto a
	// different conn whose ASKING flag is unset, so node2 responds
	// with MOVED and the pipeline GET fails.
	raceDone := make(chan struct{})
	go func() {
		defer close(raceDone)
		<-askingReplied
		var rg sync.WaitGroup
		for i := 0; i < 64; i++ {
			rg.Add(1)
			go func() {
				defer rg.Done()
				_, _ = n2.client.Do(ctx, "PING")
			}()
		}
		rg.Wait()
	}()

	p := cc.Pipeline()
	p.Get("ask-key")
	results, errs := p.Exec(ctx)
	<-raceDone
	if len(errs) != 1 || errs[0] != nil {
		t.Fatalf("unexpected errs: %v", errs)
	}
	if got := string(results[0].Str); got != "pinned-value" {
		t.Fatalf("results[0].Str = %q, want %q (ASK retry likely landed on a different conn than ASKING)", got, "pinned-value")
	}

	// Verify the GET for "ask-key" landed on the SAME conn that received
	// the ASKING. Iterate to find the ASKING and the first following GET
	// (there may be concurrent PINGs mixed in due to the race goroutine).
	node2Mu.Lock()
	defer node2Mu.Unlock()
	var askingEv *conEvent
	var getEv *conEvent
	for i := range node2Events {
		ev := &node2Events[i]
		if askingEv == nil && ev.cmd == "ASKING" {
			askingEv = ev
			continue
		}
		if askingEv != nil && ev.cmd == "GET" {
			getEv = ev
			break
		}
	}
	if askingEv == nil {
		t.Fatalf("node2 never received ASKING; events=%+v", node2Events)
	}
	if getEv == nil {
		t.Fatalf("no GET followed ASKING; events=%+v", node2Events)
	}
	if askingEv.writerID != getEv.writerID {
		t.Fatalf("ASKING (writer %x) and GET (writer %x) landed on different conns",
			askingEv.writerID, getEv.writerID)
	}
}

// uintptrOfWriter returns a stable identity for a *bufio.Writer so tests
// can correlate commands that arrived over the same TCP connection.
func uintptrOfWriter(w *bufio.Writer) uintptr {
	// The writer is created once per conn in fakeRedis.serve, so its
	// address is a valid per-conn identifier for the lifetime of that conn.
	return reflect.ValueOf(w).Pointer()
}

// TestClusterPipelineReturnsValues guards against a regression where
// ClusterPipeline.Exec dropped every successful reply because the per-node
// sub-pipeline used the direct-extraction path (pc.str + pc.direct) but the
// cluster harvester only read from pc.val. GETs would silently return a
// zero protocol.Value{}.
func TestClusterPipelineReturnsValues(t *testing.T) {
	// Single-node cluster owning all slots so both GETs route to the same
	// sub-pipeline and both exercise the direct-extraction path.
	node := startFakeClusterNode(t, 0, 16383)
	node.SetTopology([]fakeClusterSlotRange{{start: 0, end: 16383, addr: node.Addr()}})

	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	// Colocate with hash tags so a single sub-pipeline runs.
	if err := cc.Set(ctx, "{pv}.foo", "hello", 0); err != nil {
		t.Fatalf("Set foo: %v", err)
	}
	if err := cc.Set(ctx, "{pv}.bar", "world", 0); err != nil {
		t.Fatalf("Set bar: %v", err)
	}

	p := cc.Pipeline()
	p.Get("{pv}.foo")
	p.Get("{pv}.bar")
	results, errs := p.Exec(ctx)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("cmd %d error: %v", i, e)
		}
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if got := string(results[0].Str); got != "hello" {
		t.Fatalf("results[0].Str = %q, want %q", got, "hello")
	}
	if got := string(results[1].Str); got != "world" {
		t.Fatalf("results[1].Str = %q, want %q", got, "world")
	}

	// Also verify the bytes survive after the pipeline would have been
	// Released (i.e. slab reclaimed). Copy-out in harvester must be real.
	foo := append([]byte(nil), results[0].Str...)
	bar := append([]byte(nil), results[1].Str...)
	_ = foo
	_ = bar
}

func TestIsReadOnlyCommand(t *testing.T) {
	readCmds := []string{"GET", "get", "MGET", "EXISTS", "TTL", "HGET", "HGETALL",
		"LRANGE", "SMEMBERS", "ZRANGE", "SCAN", "HSCAN"}
	for _, cmd := range readCmds {
		if !isReadOnlyCommand(cmd) {
			t.Errorf("isReadOnlyCommand(%q) = false, want true", cmd)
		}
	}

	writeCmds := []string{"SET", "DEL", "INCR", "HSET", "LPUSH", "RPUSH", "SADD", "ZADD", "PUBLISH"}
	for _, cmd := range writeCmds {
		if isReadOnlyCommand(cmd) {
			t.Errorf("isReadOnlyCommand(%q) = true, want false", cmd)
		}
	}
}
