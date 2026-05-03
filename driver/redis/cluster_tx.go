package redis

import (
	"context"
	"strconv"
	"time"
)

// ClusterTx buffers commands for a MULTI/EXEC transaction on a
// [ClusterClient]. All keys must hash to the same slot; if a command targets
// a different slot, the ClusterTx records [ErrCrossSlot] and [Exec] returns
// it without contacting the server.
//
// Obtain a ClusterTx via [ClusterClient.TxPipeline]. After queuing commands,
// call [ClusterTx.Exec] to execute atomically on the owning node. Call
// [ClusterTx.Discard] to drop all queued commands without executing.
type ClusterTx struct {
	cluster *ClusterClient
	slot    int // -1 until the first key-bearing command pins it
	err     error
	cmds    []clusterTxCmd
}

// clusterTxCmd records a single queued command.
type clusterTxCmd struct {
	args []string
	kind cmdKind
	pc   *pipeCmd
}

// TxPipeline returns a [ClusterTx] that validates slot affinity for every
// queued command. If commands target different slots, [ClusterTx.Exec]
// returns [ErrCrossSlot].
func (c *ClusterClient) TxPipeline() *ClusterTx {
	return &ClusterTx{cluster: c, slot: -1}
}

// checkSlot validates that key hashes to the pinned slot. On the first call
// the slot is pinned; subsequent calls verify consistency.
func (t *ClusterTx) checkSlot(key string) {
	if t.err != nil {
		return
	}
	s := int(Slot(key))
	if t.slot == -1 {
		t.slot = s
		return
	}
	if s != t.slot {
		t.err = ErrCrossSlot
	}
}

// addCmd appends a command and returns its deferred pipeCmd.
func (t *ClusterTx) addCmd(kind cmdKind, key string, args ...string) *pipeCmd {
	t.checkSlot(key)
	pc := &pipeCmd{kind: kind}
	t.cmds = append(t.cmds, clusterTxCmd{args: args, kind: kind, pc: pc})
	return pc
}

// Exec sends all queued commands inside a MULTI/EXEC transaction to the
// cluster node that owns the pinned slot. Returns [ErrCrossSlot] if any
// command targeted a different slot than the first.
func (t *ClusterTx) Exec(ctx context.Context) error {
	if t.err != nil {
		return t.err
	}
	if len(t.cmds) == 0 {
		return nil
	}
	node := t.cluster.route(uint16(t.slot))
	if node == nil {
		node = t.cluster.anyNode()
	}
	if node == nil {
		return ErrClosed
	}
	tx, err := node.client.TxPipeline(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Discard() }()
	// Queue commands on the real Tx, keeping references to the Tx's pipeCmds
	// so we can copy results back.
	txPCs := make([]*pipeCmd, len(t.cmds))
	for i := range t.cmds {
		txPCs[i] = tx.addCmd(t.cmds[i].kind, t.cmds[i].args...)
	}
	execErr := tx.Exec(ctx)
	// Copy results from the real Tx's pipeCmds back into the ClusterTx's
	// pipeCmds so the deferred typed handles (StringCmd, IntCmd, ...) see
	// the results.
	for i := range t.cmds {
		src := txPCs[i]
		dst := t.cmds[i].pc
		dst.kind = src.kind
		dst.err = src.err
		dst.val = src.val
	}
	return execErr
}

// Discard drops all queued commands.
func (t *ClusterTx) Discard() {
	t.cmds = t.cmds[:0]
	t.slot = -1
	t.err = nil
}

// Watch validates that all keys hash to the same slot, routes to the correct
// cluster node, and delegates to the node's [Client.Watch]. The fn callback
// receives a single-node [Tx] that can queue commands normally.
func (c *ClusterClient) Watch(ctx context.Context, fn func(tx *Tx) error, keys ...string) error {
	if len(keys) == 0 {
		return ErrCrossSlot
	}
	slot := Slot(keys[0])
	for _, k := range keys[1:] {
		if Slot(k) != slot {
			return ErrCrossSlot
		}
	}
	node := c.route(slot)
	if node == nil {
		node = c.anyNode()
	}
	if node == nil {
		return ErrClosed
	}
	return node.client.Watch(ctx, fn, keys...)
}

// ---------- Typed command methods ----------

// Get enqueues GET.
func (t *ClusterTx) Get(key string) *StringCmd {
	return &StringCmd{pc: t.addCmd(kindString, key, "GET", key)}
}

// Set enqueues SET with optional EX/PX.
func (t *ClusterTx) Set(key string, value any, expiration time.Duration) *StatusCmd {
	args := []string{"SET", key, argify(value)}
	args = appendExpire(args, expiration)
	return &StatusCmd{pc: t.addCmd(kindStatus, key, args...)}
}

// SetBytes is the allocation-lean variant of [ClusterTx.Set] — uses
// unsafe.String to avoid the argify string(x) copy.
func (t *ClusterTx) SetBytes(key string, value []byte, expiration time.Duration) *StatusCmd {
	args := []string{"SET", key, unsafeStringFromBytes(value)}
	args = appendExpire(args, expiration)
	return &StatusCmd{pc: t.addCmd(kindStatus, key, args...)}
}

// Del enqueues DEL.
func (t *ClusterTx) Del(keys ...string) *IntCmd {
	for _, k := range keys {
		t.checkSlot(k)
	}
	args := append([]string{"DEL"}, keys...)
	pc := &pipeCmd{kind: kindInt}
	if len(keys) > 0 {
		t.cmds = append(t.cmds, clusterTxCmd{args: args, kind: kindInt, pc: pc})
	}
	return &IntCmd{pc: pc}
}

// Incr enqueues INCR.
func (t *ClusterTx) Incr(key string) *IntCmd {
	return &IntCmd{pc: t.addCmd(kindInt, key, "INCR", key)}
}

// Decr enqueues DECR.
func (t *ClusterTx) Decr(key string) *IntCmd {
	return &IntCmd{pc: t.addCmd(kindInt, key, "DECR", key)}
}

// Exists enqueues EXISTS.
func (t *ClusterTx) Exists(keys ...string) *IntCmd {
	for _, k := range keys {
		t.checkSlot(k)
	}
	args := append([]string{"EXISTS"}, keys...)
	pc := &pipeCmd{kind: kindInt}
	if len(keys) > 0 {
		t.cmds = append(t.cmds, clusterTxCmd{args: args, kind: kindInt, pc: pc})
	}
	return &IntCmd{pc: pc}
}

// HGet enqueues HGET.
func (t *ClusterTx) HGet(key, field string) *StringCmd {
	return &StringCmd{pc: t.addCmd(kindString, key, "HGET", key, field)}
}

// HSet enqueues HSET.
func (t *ClusterTx) HSet(key string, values ...any) *IntCmd {
	args := make([]string, 0, 2+len(values))
	args = append(args, "HSET", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	return &IntCmd{pc: t.addCmd(kindInt, key, args...)}
}

// HSetBytes enqueues single-field HSET using a zero-copy []byte value.
func (t *ClusterTx) HSetBytes(key, field string, value []byte) *IntCmd {
	args := []string{"HSET", key, field, unsafeStringFromBytes(value)}
	return &IntCmd{pc: t.addCmd(kindInt, key, args...)}
}

// LPush enqueues LPUSH.
func (t *ClusterTx) LPush(key string, values ...any) *IntCmd {
	args := make([]string, 0, 2+len(values))
	args = append(args, "LPUSH", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	return &IntCmd{pc: t.addCmd(kindInt, key, args...)}
}

// LPushBytes enqueues LPUSH with zero-copy []byte values.
func (t *ClusterTx) LPushBytes(key string, values ...[]byte) *IntCmd {
	args := make([]string, 0, 2+len(values))
	args = append(args, "LPUSH", key)
	for _, v := range values {
		args = append(args, unsafeStringFromBytes(v))
	}
	return &IntCmd{pc: t.addCmd(kindInt, key, args...)}
}

// RPush enqueues RPUSH.
func (t *ClusterTx) RPush(key string, values ...any) *IntCmd {
	args := make([]string, 0, 2+len(values))
	args = append(args, "RPUSH", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	return &IntCmd{pc: t.addCmd(kindInt, key, args...)}
}

// RPushBytes enqueues RPUSH with zero-copy []byte values.
func (t *ClusterTx) RPushBytes(key string, values ...[]byte) *IntCmd {
	args := make([]string, 0, 2+len(values))
	args = append(args, "RPUSH", key)
	for _, v := range values {
		args = append(args, unsafeStringFromBytes(v))
	}
	return &IntCmd{pc: t.addCmd(kindInt, key, args...)}
}

// SAdd enqueues SADD.
func (t *ClusterTx) SAdd(key string, members ...any) *IntCmd {
	args := make([]string, 0, 2+len(members))
	args = append(args, "SADD", key)
	for _, m := range members {
		args = append(args, argify(m))
	}
	return &IntCmd{pc: t.addCmd(kindInt, key, args...)}
}

// ZAdd enqueues ZADD.
func (t *ClusterTx) ZAdd(key string, members ...Z) *IntCmd {
	args := make([]string, 0, 2+2*len(members))
	args = append(args, "ZADD", key)
	for _, m := range members {
		args = append(args, strconv.FormatFloat(m.Score, 'f', -1, 64), argify(m.Member))
	}
	return &IntCmd{pc: t.addCmd(kindInt, key, args...)}
}

// Expire enqueues EXPIRE.
func (t *ClusterTx) Expire(key string, d time.Duration) *BoolCmd {
	secs := int64(d / time.Second)
	if secs <= 0 {
		secs = 1
	}
	return &BoolCmd{pc: t.addCmd(kindBool, key, "EXPIRE", key, strconv.FormatInt(secs, 10))}
}
