package redis

// A fake redis cluster for the pub/sub re-routing tests (celeris#887 item 31).
// It speaks just enough RESP to host PubSub conns: HELLO, CLUSTER SLOTS,
// (P/S)SUBSCRIBE and the unsubscribes with their acks, PUBLISH and SPUBLISH
// fan-out, and the three things a real Redis 7 cluster does that the tests
// need: a node that goes away (listener and conns closed), a slot that moves
// (the old owner sends "sunsubscribe <ch> 0" to the clients subscribed to a
// channel of that slot and keeps their conns), and a node that is not the
// owner of a slot answering SSUBSCRIBE with -MOVED.

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rpCluster struct {
	t     *testing.T
	nodes []*rpNode

	mu  sync.Mutex
	cur [16384]*rpNode // current owner of every slot
	old [16384]*rpNode // what a stale CLUSTER SLOTS reply still shows
	// stale is how many more CLUSTER SLOTS replies show old, not cur.
	stale int
	// slotsReplies counts the CLUSTER SLOTS replies given.
	slotsReplies atomic.Int64
}

type rpNode struct {
	cl   *rpCluster
	ln   net.Listener
	addr string

	mu       sync.Mutex
	conns    map[*rpConn]struct{}
	dead     bool
	accepted int
	// hang makes the node accept conns and never answer them (a blackholed
	// host: a dial that waits for the HELLO reply).
	hang bool
	// afterSub, when set, runs on the serve goroutine after the ack of a
	// (P/S)SUBSCRIBE was flushed, with no lock held. Guarded by mu; set it
	// with setAfterSub.
	afterSub func(n *rpNode, c *rpConn, verb string)
	// onSSub, when set, runs after an SSUBSCRIBE of ch was acked. Guarded by
	// mu; set it with setOnSSub.
	onSSub func(n *rpNode, c *rpConn, ch string)
	// subCmds counts SUBSCRIBE/PSUBSCRIBE/SSUBSCRIBE commands received.
	subCmds atomic.Int64
	// ssubLog is the arguments (channels) of every SSUBSCRIBE received, in
	// order, answered or not. Guarded by mu; read it with ssubCommands.
	ssubLog [][]string
	// ssubErrs are error replies the next SSUBSCRIBE commands get, one each,
	// before the node answers them normally. Guarded by mu.
	ssubErrs []string
}

func (n *rpNode) setAfterSub(f func(n *rpNode, c *rpConn, verb string)) {
	n.mu.Lock()
	n.afterSub = f
	n.mu.Unlock()
}

func (n *rpNode) setOnSSub(f func(n *rpNode, c *rpConn, ch string)) {
	n.mu.Lock()
	n.onSSub = f
	n.mu.Unlock()
}

// ssubCommands returns the channels of every SSUBSCRIBE the node received.
func (n *rpNode) ssubCommands() [][]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([][]string(nil), n.ssubLog...)
}

func (n *rpNode) setSSubErrs(errs ...string) {
	n.mu.Lock()
	n.ssubErrs = errs
	n.mu.Unlock()
}

type rpConn struct {
	nc    net.Conn
	w     *bufio.Writer
	wm    sync.Mutex
	proto int

	mu    sync.Mutex
	subs  map[string]struct{}
	psubs map[string]struct{}
	ssubs map[string]struct{}
}

func newRPCluster(t *testing.T, n int) *rpCluster {
	t.Helper()
	cl := &rpCluster{t: t}
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		nd := &rpNode{cl: cl, ln: ln, addr: ln.Addr().String(), conns: map[*rpConn]struct{}{}}
		cl.nodes = append(cl.nodes, nd)
	}
	for s := 0; s < 16384; s++ {
		o := cl.nodes[s*n/16384]
		cl.cur[s] = o
		cl.old[s] = o
	}
	for _, nd := range cl.nodes {
		go nd.accept()
	}
	t.Cleanup(func() {
		for _, nd := range cl.nodes {
			nd.kill()
		}
	})
	return cl
}

func (cl *rpCluster) addrs() []string {
	out := make([]string, len(cl.nodes))
	for i, n := range cl.nodes {
		out[i] = n.addr
	}
	return out
}

// owner returns the current owner of slot.
func (cl *rpCluster) owner(slot int) *rpNode {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.cur[slot]
}

// moveSlot gives slot to to. The old owner tells its subscribers of channels
// in that slot with "sunsubscribe <ch> 0" and keeps their conns, as Redis 7
// does. staleReplies is how many CLUSTER SLOTS replies still show the old
// table.
func (cl *rpCluster) moveSlot(slot int, to *rpNode, staleReplies int) {
	cl.mu.Lock()
	from := cl.cur[slot]
	cl.old = cl.cur
	cl.cur[slot] = to
	cl.stale = staleReplies
	cl.mu.Unlock()
	from.sunsubscribeSlot(slot)
}

// failover gives every slot of from to to, as a promoted replica does.
func (cl *rpCluster) failover(from, to *rpNode, staleReplies int) {
	cl.mu.Lock()
	cl.old = cl.cur
	for s := range cl.cur {
		if cl.cur[s] == from {
			cl.cur[s] = to
		}
	}
	cl.stale = staleReplies
	cl.mu.Unlock()
}

// publish is PUBLISH: every live node delivers to its subscribers of ch.
// It returns the number of deliveries.
func (cl *rpCluster) publish(ch, payload string) int {
	n := 0
	for _, nd := range cl.nodes {
		n += nd.deliver("message", ch, payload, func(c *rpConn) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			_, ok := c.subs[ch]
			return ok
		})
	}
	return n
}

// spublish is SPUBLISH: the owner of the slot delivers to its subscribers.
func (cl *rpCluster) spublish(ch, payload string) int {
	return cl.owner(int(Slot(ch))).deliver("smessage", ch, payload, func(c *rpConn) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.ssubs[ch]
		return ok
	})
}

// subscribers counts the live conns of every node that hold any subscription
// of the kind ("sub", "psub", "ssub").
func (cl *rpCluster) subscribers(kind string) int {
	n := 0
	for _, nd := range cl.nodes {
		n += nd.subscribers(kind)
	}
	return n
}

// liveConns counts every open conn of every node.
func (cl *rpCluster) liveConns() int {
	n := 0
	for _, nd := range cl.nodes {
		nd.mu.Lock()
		n += len(nd.conns)
		nd.mu.Unlock()
	}
	return n
}

// holder returns the node holding a conn subscribed with kind, or nil.
func (cl *rpCluster) holder(kind string) *rpNode {
	for _, nd := range cl.nodes {
		if nd.subscribers(kind) > 0 {
			return nd
		}
	}
	return nil
}

func (n *rpNode) accept() {
	for {
		nc, err := n.ln.Accept()
		if err != nil {
			return
		}
		n.mu.Lock()
		if n.dead {
			n.mu.Unlock()
			_ = nc.Close()
			return
		}
		n.accepted++
		hang := n.hang
		c := &rpConn{nc: nc, w: bufio.NewWriter(nc), proto: 2,
			subs: map[string]struct{}{}, psubs: map[string]struct{}{}, ssubs: map[string]struct{}{}}
		n.conns[c] = struct{}{}
		n.mu.Unlock()
		if hang {
			continue
		}
		go n.serve(c)
	}
}

// kill takes the node away: the listener and every conn close.
func (n *rpNode) kill() {
	n.mu.Lock()
	n.dead = true
	conns := n.conns
	n.conns = map[*rpConn]struct{}{}
	n.mu.Unlock()
	_ = n.ln.Close()
	for c := range conns {
		_ = c.nc.Close()
	}
}

func (n *rpNode) acceptedConns() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.accepted
}

func (n *rpNode) forget(c *rpConn) {
	n.mu.Lock()
	delete(n.conns, c)
	n.mu.Unlock()
}

func (n *rpNode) subscribers(kind string) int {
	n.mu.Lock()
	cs := make([]*rpConn, 0, len(n.conns))
	for c := range n.conns {
		cs = append(cs, c)
	}
	n.mu.Unlock()
	cnt := 0
	for _, c := range cs {
		c.mu.Lock()
		var l int
		switch kind {
		case "sub":
			l = len(c.subs)
		case "psub":
			l = len(c.psubs)
		default:
			l = len(c.ssubs)
		}
		c.mu.Unlock()
		if l > 0 {
			cnt++
		}
	}
	return cnt
}

func (n *rpNode) conn(pred func(*rpConn) bool) *rpConn {
	n.mu.Lock()
	defer n.mu.Unlock()
	for c := range n.conns {
		if pred(c) {
			return c
		}
	}
	return nil
}

func (n *rpNode) deliver(kind, ch, payload string, match func(*rpConn) bool) int {
	n.mu.Lock()
	cs := make([]*rpConn, 0, len(n.conns))
	for c := range n.conns {
		cs = append(cs, c)
	}
	n.mu.Unlock()
	cnt := 0
	for _, c := range cs {
		if !match(c) {
			continue
		}
		c.wm.Lock()
		c.pushStrs(kind, ch, payload)
		_ = c.w.Flush()
		c.wm.Unlock()
		cnt++
	}
	return cnt
}

// sunsubscribeSlot is what a node does when it stops owning slot: every
// subscriber of a channel of the slot gets "sunsubscribe <ch> 0" and keeps
// its conn.
func (n *rpNode) sunsubscribeSlot(slot int) {
	n.mu.Lock()
	cs := make([]*rpConn, 0, len(n.conns))
	for c := range n.conns {
		cs = append(cs, c)
	}
	n.mu.Unlock()
	for _, c := range cs {
		c.mu.Lock()
		var chs []string
		for ch := range c.ssubs {
			if int(Slot(ch)) == slot {
				chs = append(chs, ch)
				delete(c.ssubs, ch)
			}
		}
		left := len(c.ssubs)
		c.mu.Unlock()
		for _, ch := range chs {
			c.wm.Lock()
			c.pushAck("sunsubscribe", ch, int64(left))
			_ = c.w.Flush()
			c.wm.Unlock()
		}
	}
}

// pushStrs writes a push of bulk strings (RESP3 ">", RESP2 array). The caller
// holds wm.
func (c *rpConn) pushStrs(items ...string) {
	if c.proto == 3 {
		writePush(c.w, items...)
		return
	}
	writeArrayBulks(c.w, items...)
}

// pushAck writes a subscribe-family ack: [kind, channel, count]. The caller
// holds wm.
func (c *rpConn) pushAck(kind, ch string, count int64) {
	if c.proto == 3 {
		_ = c.w.WriteByte('>')
	} else {
		_ = c.w.WriteByte('*')
	}
	_, _ = c.w.WriteString("3\r\n")
	writeBulk(c.w, kind)
	if ch == "" {
		writeNullBulk(c.w)
	} else {
		writeBulk(c.w, ch)
	}
	writeInt(c.w, count)
}

func (n *rpNode) serve(c *rpConn) {
	defer func() {
		_ = c.nc.Close()
		n.forget(c)
	}()
	r := bufio.NewReader(c.nc)
	var args []string
	for {
		cmd, err := readCommandInto(r, args[:0])
		if err != nil {
			return
		}
		args = cmd
		if len(cmd) == 0 {
			continue
		}
		verb := strings.ToUpper(cmd[0])
		c.wm.Lock()
		after := n.handle(c, verb, cmd)
		err = c.w.Flush()
		c.wm.Unlock()
		if err != nil {
			return
		}
		if after != nil {
			after()
		}
	}
}

// handle answers one command. The caller holds c.wm and flushes. The returned
// func, when not nil, runs after the flush with no lock held.
func (n *rpNode) handle(c *rpConn, verb string, cmd []string) func() {
	switch verb {
	case "HELLO":
		proto := 2
		if len(cmd) > 1 && cmd[1] == "3" {
			proto = 3
		}
		c.proto = proto
		handleHELLO(c.w, proto)
	case "PING":
		writeSimple(c.w, "PONG")
	case "CLUSTER":
		if len(cmd) >= 2 && strings.EqualFold(cmd[1], "SLOTS") {
			n.cl.writeSlots(c.w)
			return nil
		}
		writeError(c.w, "ERR unknown cluster subcommand")
	case "SUBSCRIBE", "PSUBSCRIBE":
		n.subCmds.Add(1)
		c.mu.Lock()
		set, kind := c.subs, "subscribe"
		if verb == "PSUBSCRIBE" {
			set, kind = c.psubs, "psubscribe"
		}
		for _, ch := range cmd[1:] {
			set[ch] = struct{}{}
			c.pushAck(kind, ch, int64(len(set)))
		}
		c.mu.Unlock()
		n.mu.Lock()
		f := n.afterSub
		n.mu.Unlock()
		if f != nil {
			return func() { f(n, c, verb) }
		}
	case "UNSUBSCRIBE", "PUNSUBSCRIBE":
		c.mu.Lock()
		set, kind := c.subs, "unsubscribe"
		if verb == "PUNSUBSCRIBE" {
			set, kind = c.psubs, "punsubscribe"
		}
		names := cmd[1:]
		if len(names) == 0 {
			for k := range set {
				names = append(names, k)
			}
		}
		for _, ch := range names {
			delete(set, ch)
			c.pushAck(kind, ch, int64(len(set)))
		}
		c.mu.Unlock()
	case "ROLE":
		writeArrayHeader(c.w, 3)
		writeBulk(c.w, "master")
		writeInt(c.w, 0)
		writeArrayHeader(c.w, 0)
	case "SSUBSCRIBE":
		n.subCmds.Add(1)
		n.mu.Lock()
		n.ssubLog = append(n.ssubLog, append([]string(nil), cmd[1:]...))
		var forced string
		if len(n.ssubErrs) > 0 {
			forced, n.ssubErrs = n.ssubErrs[0], n.ssubErrs[1:]
		}
		n.mu.Unlock()
		if forced != "" {
			writeError(c.w, forced)
			return nil
		}
		for _, ch := range cmd[1:] {
			if Slot(ch) != Slot(cmd[1]) {
				writeError(c.w, "CROSSSLOT Keys in request don't hash to the same slot")
				return nil
			}
		}
		for _, ch := range cmd[1:] {
			slot := int(Slot(ch))
			if o := n.cl.owner(slot); o != n {
				writeError(c.w, fmt.Sprintf("MOVED %d %s", slot, o.addr))
				return nil
			}
		}
		c.mu.Lock()
		for _, ch := range cmd[1:] {
			c.ssubs[ch] = struct{}{}
			c.pushAck("ssubscribe", ch, int64(len(c.ssubs)))
		}
		c.mu.Unlock()
		chs := append([]string(nil), cmd[1:]...)
		n.mu.Lock()
		f, g := n.afterSub, n.onSSub
		n.mu.Unlock()
		if f != nil || g != nil {
			return func() {
				if f != nil {
					f(n, c, verb)
				}
				if g != nil {
					for _, ch := range chs {
						g(n, c, ch)
					}
				}
			}
		}
	case "SUNSUBSCRIBE":
		// Like Redis 7 (checked on 8.0.2), a command across slots is refused.
		for _, ch := range cmd[1:] {
			if Slot(ch) != Slot(cmd[1]) {
				writeError(c.w, "CROSSSLOT Keys in request don't hash to the same slot")
				return nil
			}
		}
		// Like Redis 7, a node that does not serve the slot of a channel it
		// holds no subscription for answers -MOVED.
		for _, ch := range cmd[1:] {
			c.mu.Lock()
			_, held := c.ssubs[ch]
			c.mu.Unlock()
			if slot := int(Slot(ch)); !held && n.cl.owner(slot) != n {
				writeError(c.w, fmt.Sprintf("MOVED %d %s", slot, n.cl.owner(slot).addr))
				return nil
			}
		}
		c.mu.Lock()
		names := cmd[1:]
		if len(names) == 0 {
			for k := range c.ssubs {
				names = append(names, k)
			}
		}
		for _, ch := range names {
			delete(c.ssubs, ch)
			c.pushAck("sunsubscribe", ch, int64(len(c.ssubs)))
		}
		c.mu.Unlock()
	default:
		writeSimple(c.w, "OK")
	}
	return nil
}

// writeSlots answers CLUSTER SLOTS from the current table, or the old one
// while stale replies are owed.
func (cl *rpCluster) writeSlots(w *bufio.Writer) {
	cl.slotsReplies.Add(1)
	cl.mu.Lock()
	tab := cl.cur
	if cl.stale > 0 {
		cl.stale--
		tab = cl.old
	}
	cl.mu.Unlock()
	type rng struct {
		lo, hi int
		o      *rpNode
	}
	var rs []rng
	for s := 0; s < 16384; s++ {
		if tab[s] == nil {
			continue
		}
		if k := len(rs) - 1; k >= 0 && rs[k].o == tab[s] && rs[k].hi == s-1 {
			rs[k].hi = s
			continue
		}
		rs = append(rs, rng{s, s, tab[s]})
	}
	writeArrayHeader(w, len(rs))
	for _, r := range rs {
		host, port, _ := splitHostPort(r.o.addr)
		p, _ := strconv.Atoi(port)
		writeArrayHeader(w, 3)
		writeInt(w, int64(r.lo))
		writeInt(w, int64(r.hi))
		writeArrayHeader(w, 3)
		writeBulk(w, host)
		writeInt(w, int64(p))
		writeBulk(w, "node-"+r.o.addr)
	}
}

// ---- collectors and polling ----

// rpCollector reads a PubSub's channel and counts each payload.
type rpCollector struct {
	mu     sync.Mutex
	counts map[string]int
	closed atomic.Bool
}

func collect(ps *PubSub) *rpCollector {
	rc := &rpCollector{counts: map[string]int{}}
	go func() {
		for m := range ps.Channel() {
			rc.mu.Lock()
			rc.counts[m.Payload]++
			rc.mu.Unlock()
		}
		rc.closed.Store(true)
	}()
	return rc
}

func (rc *rpCollector) count(payload string) int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.counts[payload]
}

// poll waits for cond, failing the test after d.
func poll(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// rpLoopIdle reports whether ps has no reconnect loop running. After Close the
// loop of this PubSub must end (checking the PubSub's own flag, not every
// goroutine of the process, keeps other tests' leftovers out of it).
func rpLoopIdle(ps *PubSub) bool {
	ps.reconnectingMu.Lock()
	defer ps.reconnectingMu.Unlock()
	return !ps.reconnecting
}
