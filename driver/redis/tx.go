package redis

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

// Tx is a MULTI/EXEC transaction. Commands are buffered into a single wire
// write (MULTI + cmds + EXEC) and dispatched atomically. Per-command typed
// results are reachable via the *Cmd handles returned by each method — they
// populate only after Exec returns.
//
// A Tx pins one connection for its entire lifetime. Exec releases the conn;
// Discard sends DISCARD and releases. Forgetting to call either leaks the
// conn until the pool's idle reaper closes it, so a sync.Pool-style
// "always defer tx.Close()" is recommended — Close is an alias for Discard
// that is a no-op after Exec.
type Tx struct {
	client *Client
	conn   *redisConn
	cmds   []*pipeCmd
	w      *protocol.Writer
	done   bool
}

// TxPipeline opens a MULTI/EXEC transaction. The returned Tx pins a
// connection from the cmd pool until Exec or Discard. Typed command methods
// (Get, Set, Incr, ...) queue commands onto the Tx; Exec flushes them all
// under a single MULTI/EXEC.
func (c *Client) TxPipeline(ctx context.Context) (*Tx, error) {
	conn, err := c.pool.acquireCmd(ctx, workerFromCtx(ctx))
	if err != nil {
		return nil, err
	}
	conn.dirty.Store(true)
	return &Tx{
		client: c,
		conn:   conn,
		w:      protocol.NewWriter(),
	}, nil
}

// Watch issues WATCH on the pinned conn before queuing any commands. Must
// be called before any buffered command. Returns an error if the reply is
// not +OK or keys is empty.
func (tx *Tx) Watch(ctx context.Context, keys ...string) error {
	if tx.done {
		return errors.New("celeris-redis: Tx already executed")
	}
	if len(keys) == 0 {
		return errors.New("celeris-redis: Watch requires at least one key")
	}
	if len(tx.cmds) > 0 {
		return errors.New("celeris-redis: Watch must be called before any queued command")
	}
	args := append([]string{"WATCH"}, keys...)
	req, err := tx.conn.exec(ctx, args...)
	if err != nil {
		return err
	}
	defer tx.conn.releaseResult(req)
	return req.resultErr
}

// Unwatch drops all watched keys on the pinned conn.
func (tx *Tx) Unwatch(ctx context.Context) error {
	if tx.done {
		return errors.New("celeris-redis: Tx already executed")
	}
	req, err := tx.conn.exec(ctx, "UNWATCH")
	if err != nil {
		return err
	}
	defer tx.conn.releaseResult(req)
	return req.resultErr
}

// addCmd appends args to the tx buffer and returns a tracking cmd.
func (tx *Tx) addCmd(kind cmdKind, args ...string) *pipeCmd {
	tx.w.AppendCommand(args...)
	pc := &pipeCmd{kind: kind}
	tx.cmds = append(tx.cmds, pc)
	return pc
}

func (tx *Tx) addCmd2(kind cmdKind, a0, a1 string) *pipeCmd {
	tx.w.AppendCommand2(a0, a1)
	pc := &pipeCmd{kind: kind}
	tx.cmds = append(tx.cmds, pc)
	return pc
}

func (tx *Tx) addCmd3(kind cmdKind, a0, a1, a2 string) *pipeCmd {
	tx.w.AppendCommand3(a0, a1, a2)
	pc := &pipeCmd{kind: kind}
	tx.cmds = append(tx.cmds, pc)
	return pc
}

// Exec sends MULTI, every queued command, and EXEC in a single write, then
// dispatches the EXEC array's elements to the typed result slots.
//
// Behaviour:
//   - The MULTI reply is +OK; each queued command's immediate reply is
//     +QUEUED; the final EXEC reply is an array of N results.
//   - If EXEC returns a null array (RESP2 *-1 / RESP3 _), the transaction
//     was aborted (e.g. a WATCHed key changed) — every typed result will
//     return [ErrTxAborted].
//   - If a single command errors server-side (WRONGTYPE, ...), that command's
//     result returns the error but the others complete normally.
//   - If the server returns -EXECABORT, every typed result returns that
//     error.
//   - After Exec, the tx is done: further Watch/Exec/command calls return an
//     error. The conn is returned to the pool.
func (tx *Tx) Exec(ctx context.Context) error {
	if tx.done {
		return errors.New("celeris-redis: Tx already executed")
	}
	tx.done = true
	defer func() {
		tx.conn.dirty.Store(false)
		tx.client.pool.releaseCmd(tx.conn)
	}()

	n := len(tx.cmds)
	if n == 0 {
		// No commands: avoid sending an empty transaction. Nothing to do.
		return nil
	}
	// Build: MULTI\r\n <buffered cmds> \r\n EXEC\r\n
	multi := protocol.NewWriter().AppendCommand1("MULTI")
	exec := protocol.NewWriter().AppendCommand1("EXEC")
	payload := make([]byte, 0, len(multi)+len(tx.w.Bytes())+len(exec))
	payload = append(payload, multi...)
	payload = append(payload, tx.w.Bytes()...)
	payload = append(payload, exec...)

	// Expect n+2 replies: MULTI (+OK), n per-cmd (+QUEUED), EXEC (array).
	total := n + 2
	reqs, err := tx.conn.execMany(ctx, payload, total)
	if err != nil {
		for _, pc := range tx.cmds {
			pc.err = err
		}
		return err
	}
	multiReq := reqs[0]
	defer tx.conn.releaseResult(multiReq)
	if multiReq.resultErr != nil {
		for _, pc := range tx.cmds {
			pc.err = multiReq.resultErr
		}
		return multiReq.resultErr
	}
	// queued replies — consume and release; any *QUEUED status is expected.
	// A server error here (e.g. unknown command) surfaces on the individual
	// command.
	for i := 0; i < n; i++ {
		qr := reqs[1+i]
		if qr.resultErr != nil {
			tx.cmds[i].err = qr.resultErr
		}
		tx.conn.releaseResult(qr)
	}
	execReq := reqs[1+n]
	defer tx.conn.releaseResult(execReq)
	if execReq.resultErr != nil {
		// -EXECABORT or similar: every typed result reports abort.
		for _, pc := range tx.cmds {
			if pc.err == nil {
				pc.err = ErrTxAborted
			}
		}
		return execReq.resultErr
	}
	// Null array means WATCH aborted the transaction.
	if execReq.result.Type == protocol.TyNull {
		for _, pc := range tx.cmds {
			pc.err = ErrTxAborted
		}
		return ErrTxAborted
	}
	if execReq.result.Type != protocol.TyArray && execReq.result.Type != protocol.TySet {
		err := errors.New("celeris-redis: EXEC reply is not an array")
		for _, pc := range tx.cmds {
			if pc.err == nil {
				pc.err = err
			}
		}
		return err
	}
	elems := execReq.result.Array
	if len(elems) != n {
		err := errors.New("celeris-redis: EXEC reply count mismatch")
		for _, pc := range tx.cmds {
			if pc.err == nil {
				pc.err = err
			}
		}
		return err
	}
	var firstErr error
	for i, e := range elems {
		if e.Type == protocol.TyError || e.Type == protocol.TyBlobErr {
			tx.cmds[i].err = parseError(e.Str)
			if firstErr == nil {
				firstErr = tx.cmds[i].err
			}
			continue
		}
		v := e
		tx.cmds[i].val = &v
	}
	return firstErr
}

// Discard aborts the transaction. If Exec was not called, DISCARD is sent
// to the server to cancel MULTI state, and the conn is released. After
// Discard, the Tx is done.
func (tx *Tx) Discard() error {
	if tx.done {
		return nil
	}
	tx.done = true
	defer tx.client.pool.releaseCmd(tx.conn)
	// Nothing was queued server-side yet (MULTI is only sent in Exec). Just
	// clear the dirty flag so resetSession does not send a stray DISCARD.
	tx.conn.dirty.Store(false)
	return nil
}

// ---------- Tx commands (mirror Pipeline) ----------

// Get enqueues GET.
func (tx *Tx) Get(key string) *StringCmd {
	return &StringCmd{pc: tx.addCmd2(kindString, "GET", key)}
}

// Set enqueues SET with optional EX/PX.
func (tx *Tx) Set(key string, value any, expiration time.Duration) *StatusCmd {
	args := []string{"SET", key, argify(value)}
	args = appendExpire(args, expiration)
	return &StatusCmd{pc: tx.addCmd(kindStatus, args...)}
}

// Del enqueues DEL.
func (tx *Tx) Del(keys ...string) *IntCmd {
	args := append([]string{"DEL"}, keys...)
	return &IntCmd{pc: tx.addCmd(kindInt, args...)}
}

// Incr enqueues INCR.
func (tx *Tx) Incr(key string) *IntCmd {
	return &IntCmd{pc: tx.addCmd2(kindInt, "INCR", key)}
}

// Decr enqueues DECR.
func (tx *Tx) Decr(key string) *IntCmd {
	return &IntCmd{pc: tx.addCmd2(kindInt, "DECR", key)}
}

// Exists enqueues EXISTS.
func (tx *Tx) Exists(keys ...string) *IntCmd {
	args := append([]string{"EXISTS"}, keys...)
	return &IntCmd{pc: tx.addCmd(kindInt, args...)}
}

// HGet enqueues HGET.
func (tx *Tx) HGet(key, field string) *StringCmd {
	return &StringCmd{pc: tx.addCmd3(kindString, "HGET", key, field)}
}

// HSet enqueues HSET.
func (tx *Tx) HSet(key string, values ...any) *IntCmd {
	args := make([]string, 0, 2+len(values))
	args = append(args, "HSET", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	return &IntCmd{pc: tx.addCmd(kindInt, args...)}
}

// LPush enqueues LPUSH.
func (tx *Tx) LPush(key string, values ...any) *IntCmd {
	args := make([]string, 0, 2+len(values))
	args = append(args, "LPUSH", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	return &IntCmd{pc: tx.addCmd(kindInt, args...)}
}

// RPush enqueues RPUSH.
func (tx *Tx) RPush(key string, values ...any) *IntCmd {
	args := make([]string, 0, 2+len(values))
	args = append(args, "RPUSH", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	return &IntCmd{pc: tx.addCmd(kindInt, args...)}
}

// SAdd enqueues SADD.
func (tx *Tx) SAdd(key string, members ...any) *IntCmd {
	args := make([]string, 0, 2+len(members))
	args = append(args, "SADD", key)
	for _, m := range members {
		args = append(args, argify(m))
	}
	return &IntCmd{pc: tx.addCmd(kindInt, args...)}
}

// ZAdd enqueues ZADD.
func (tx *Tx) ZAdd(key string, members ...Z) *IntCmd {
	args := make([]string, 0, 2+2*len(members))
	args = append(args, "ZADD", key)
	for _, m := range members {
		args = append(args, strconv.FormatFloat(m.Score, 'f', -1, 64), argify(m.Member))
	}
	return &IntCmd{pc: tx.addCmd(kindInt, args...)}
}

// Expire enqueues EXPIRE.
func (tx *Tx) Expire(key string, d time.Duration) *BoolCmd {
	secs := int64(d / time.Second)
	if secs <= 0 {
		secs = 1
	}
	return &BoolCmd{pc: tx.addCmd3(kindBool, "EXPIRE", key, strconv.FormatInt(secs, 10))}
}
