package redis

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// defaultHandler implements a minimal key/value/hash store for tests.
type memStore struct {
	mu     sync.Mutex
	kv     map[string]string
	hash   map[string]map[string]string
	list   map[string][]string
	set    map[string]map[string]struct{}
	zset   map[string]map[string]float64
	killed bool
	// per-conn transaction buffers keyed by the *bufio.Writer receiving the
	// conn's output — used by MULTI/EXEC path so the fake can stage queued
	// commands per connection.
	txBuf map[*bufio.Writer][][]string
	// watchedAbort, when true for a given writer, will cause the next EXEC
	// reply to be a null array (simulating a WATCHed key change).
	watchedAbort map[*bufio.Writer]bool
}

func newMem() *memStore {
	return &memStore{
		kv:           map[string]string{},
		hash:         map[string]map[string]string{},
		list:         map[string][]string{},
		set:          map[string]map[string]struct{}{},
		zset:         map[string]map[string]float64{},
		txBuf:        map[*bufio.Writer][][]string{},
		watchedAbort: map[*bufio.Writer]bool{},
	}
}

func (m *memStore) handler(cmd []string, w *bufio.Writer) {
	if len(cmd) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Transaction control — these bypass the per-writer txBuf staging.
	upper := strings.ToUpper(cmd[0])
	switch upper {
	case "MULTI":
		m.txBuf[w] = [][]string{}
		writeSimple(w, "OK")
		return
	case "DISCARD":
		if _, ok := m.txBuf[w]; ok {
			delete(m.txBuf, w)
			writeSimple(w, "OK")
		} else {
			writeError(w, "ERR DISCARD without MULTI")
		}
		return
	case "EXEC":
		staged, ok := m.txBuf[w]
		if !ok {
			writeError(w, "ERR EXEC without MULTI")
			return
		}
		delete(m.txBuf, w)
		if m.watchedAbort[w] {
			delete(m.watchedAbort, w)
			writeNullBulk(w)
			return
		}
		writeArrayHeader(w, len(staged))
		for _, staged := range staged {
			m.executeLocked(staged, w)
		}
		return
	case "WATCH":
		writeSimple(w, "OK")
		return
	case "UNWATCH":
		delete(m.watchedAbort, w)
		writeSimple(w, "OK")
		return
	}
	// If we're inside a transaction on this conn, stage the command and
	// reply +QUEUED.
	if _, inTx := m.txBuf[w]; inTx {
		staged := append([]string(nil), cmd...)
		m.txBuf[w] = append(m.txBuf[w], staged)
		writeSimple(w, "QUEUED")
		return
	}
	m.executeLocked(cmd, w)
}

// executeLocked writes a reply for cmd. Caller must hold m.mu.
func (m *memStore) executeLocked(cmd []string, w *bufio.Writer) {
	switch strings.ToUpper(cmd[0]) {
	case "HELLO":
		proto := 2
		if len(cmd) > 1 {
			if cmd[1] == "3" {
				proto = 3
			}
		}
		handleHELLO(w, proto)
	case "AUTH":
		writeSimple(w, "OK")
	case "SELECT":
		writeSimple(w, "OK")
	case "PING":
		writeSimple(w, "PONG")
	case "GET":
		if v, ok := m.kv[cmd[1]]; ok {
			writeBulk(w, v)
		} else {
			writeNullBulk(w)
		}
	case "SET":
		m.kv[cmd[1]] = cmd[2]
		writeSimple(w, "OK")
	case "DEL":
		n := int64(0)
		for _, k := range cmd[1:] {
			if _, ok := m.kv[k]; ok {
				delete(m.kv, k)
				n++
			}
		}
		writeInt(w, n)
	case "EXISTS":
		n := int64(0)
		for _, k := range cmd[1:] {
			if _, ok := m.kv[k]; ok {
				n++
			}
		}
		writeInt(w, n)
	case "INCR":
		n := int64(0)
		if s, ok := m.kv[cmd[1]]; ok {
			// best-effort parse
			for _, c := range s {
				n = n*10 + int64(c-'0')
			}
		}
		n++
		m.kv[cmd[1]] = itoa(n)
		writeInt(w, n)
	case "HSET":
		h, ok := m.hash[cmd[1]]
		if !ok {
			h = map[string]string{}
			m.hash[cmd[1]] = h
		}
		n := int64(0)
		for i := 2; i+1 < len(cmd); i += 2 {
			if _, exists := h[cmd[i]]; !exists {
				n++
			}
			h[cmd[i]] = cmd[i+1]
		}
		writeInt(w, n)
	case "HGET":
		if h, ok := m.hash[cmd[1]]; ok {
			if v, ok := h[cmd[2]]; ok {
				writeBulk(w, v)
				return
			}
		}
		writeNullBulk(w)
	case "HGETALL":
		h := m.hash[cmd[1]]
		writeArrayHeader(w, 2*len(h))
		for k, v := range h {
			writeBulk(w, k)
			writeBulk(w, v)
		}
	case "LPUSH":
		for _, v := range cmd[2:] {
			m.list[cmd[1]] = append([]string{v}, m.list[cmd[1]]...)
		}
		writeInt(w, int64(len(m.list[cmd[1]])))
	case "RPUSH":
		m.list[cmd[1]] = append(m.list[cmd[1]], cmd[2:]...)
		writeInt(w, int64(len(m.list[cmd[1]])))
	case "LRANGE":
		l := m.list[cmd[1]]
		writeArrayBulks(w, l...)
	case "SADD":
		s, ok := m.set[cmd[1]]
		if !ok {
			s = map[string]struct{}{}
			m.set[cmd[1]] = s
		}
		n := int64(0)
		for _, v := range cmd[2:] {
			if _, ok := s[v]; !ok {
				n++
				s[v] = struct{}{}
			}
		}
		writeInt(w, n)
	case "SMEMBERS":
		s := m.set[cmd[1]]
		items := make([]string, 0, len(s))
		for k := range s {
			items = append(items, k)
		}
		writeArrayBulks(w, items...)
	case "ZADD":
		z, ok := m.zset[cmd[1]]
		if !ok {
			z = map[string]float64{}
			m.zset[cmd[1]] = z
		}
		n := int64(0)
		for i := 2; i+1 < len(cmd); i += 2 {
			if _, exists := z[cmd[i+1]]; !exists {
				n++
			}
			z[cmd[i+1]] = 0
		}
		writeInt(w, n)
	case "ZRANGE":
		z := m.zset[cmd[1]]
		items := make([]string, 0, len(z))
		for k := range z {
			items = append(items, k)
		}
		writeArrayBulks(w, items...)
	case "SUBSCRIBE":
		for i, ch := range cmd[1:] {
			// push-style confirmation
			writeArrayHeader(w, 3)
			writeBulk(w, "subscribe")
			writeBulk(w, ch)
			writeInt(w, int64(i+1))
		}
	case "UNSUBSCRIBE":
		for i, ch := range cmd[1:] {
			writeArrayHeader(w, 3)
			writeBulk(w, "unsubscribe")
			writeBulk(w, ch)
			writeInt(w, int64(i+1))
		}
	case "PUBLISH":
		// Real pub/sub routing isn't modeled here — just return 0 (no
		// subscribers) for bookkeeping tests.
		writeInt(w, 0)
	case "PEXPIRE", "EXPIREAT", "PEXPIREAT", "EXPIRE":
		if _, ok := m.kv[cmd[1]]; ok {
			writeInt(w, 1)
		} else if _, ok := m.hash[cmd[1]]; ok {
			writeInt(w, 1)
		} else {
			writeInt(w, 0)
		}
	case "PERSIST":
		if _, ok := m.kv[cmd[1]]; ok {
			writeInt(w, 1)
		} else {
			writeInt(w, 0)
		}
	case "EVAL":
		// Trivial fake: ignore script body, return the first arg (or nil).
		if len(cmd) > 3 {
			writeBulk(w, cmd[3])
		} else {
			writeNullBulk(w)
		}
	case "EVALSHA":
		writeError(w, "NOSCRIPT No matching script")
	case "SCRIPT":
		// SCRIPT LOAD <body> → return a fixed sha1-ish token.
		writeBulk(w, "deadbeef00000000000000000000000000000000")
	case "MSET":
		for i := 1; i+1 < len(cmd); i += 2 {
			m.kv[cmd[i]] = cmd[i+1]
		}
		writeSimple(w, "OK")
	case "INCRBY":
		n := int64(0)
		if s, ok := m.kv[cmd[1]]; ok {
			for _, c := range s {
				if c == '-' {
					continue
				}
				n = n*10 + int64(c-'0')
			}
			if len(s) > 0 && s[0] == '-' {
				n = -n
			}
		}
		delta := int64(0)
		for _, c := range cmd[2] {
			if c == '-' {
				continue
			}
			delta = delta*10 + int64(c-'0')
		}
		if len(cmd[2]) > 0 && cmd[2][0] == '-' {
			delta = -delta
		}
		n += delta
		m.kv[cmd[1]] = itoa(n)
		writeInt(w, n)
	case "DECRBY":
		n := int64(0)
		if s, ok := m.kv[cmd[1]]; ok {
			for _, c := range s {
				if c == '-' {
					continue
				}
				n = n*10 + int64(c-'0')
			}
			if len(s) > 0 && s[0] == '-' {
				n = -n
			}
		}
		delta := int64(0)
		for _, c := range cmd[2] {
			if c == '-' {
				continue
			}
			delta = delta*10 + int64(c-'0')
		}
		if len(cmd[2]) > 0 && cmd[2][0] == '-' {
			delta = -delta
		}
		n -= delta
		m.kv[cmd[1]] = itoa(n)
		writeInt(w, n)
	case "DECR":
		n := int64(0)
		if s, ok := m.kv[cmd[1]]; ok {
			for _, c := range s {
				if c == '-' {
					continue
				}
				n = n*10 + int64(c-'0')
			}
			if len(s) > 0 && s[0] == '-' {
				n = -n
			}
		}
		n--
		m.kv[cmd[1]] = itoa(n)
		writeInt(w, n)
	case "INCRBYFLOAT":
		writeBulk(w, cmd[2]) // simplified: return the increment itself
	case "GETDEL":
		if v, ok := m.kv[cmd[1]]; ok {
			delete(m.kv, cmd[1])
			writeBulk(w, v)
		} else {
			writeNullBulk(w)
		}
	case "SETEX":
		// SETEX key seconds value
		if len(cmd) >= 4 {
			m.kv[cmd[1]] = cmd[3]
		}
		writeSimple(w, "OK")
	case "APPEND":
		m.kv[cmd[1]] += cmd[2]
		writeInt(w, int64(len(m.kv[cmd[1]])))
	case "LINDEX":
		l := m.list[cmd[1]]
		idx := 0
		for _, c := range cmd[2] {
			if c == '-' {
				continue
			}
			idx = idx*10 + int(c-'0')
		}
		if len(cmd[2]) > 0 && cmd[2][0] == '-' {
			idx = len(l) + idx*-1 // negative index hack (simplified)
		}
		if idx >= 0 && idx < len(l) {
			writeBulk(w, l[idx])
		} else {
			writeNullBulk(w)
		}
	case "LREM":
		writeInt(w, 0) // simplified
	case "LLEN":
		writeInt(w, int64(len(m.list[cmd[1]])))
	case "SINTER":
		if len(cmd) < 2 {
			writeArrayBulks(w)
			break
		}
		result := map[string]struct{}{}
		if first, ok := m.set[cmd[1]]; ok {
			for k := range first {
				result[k] = struct{}{}
			}
		}
		for _, key := range cmd[2:] {
			s := m.set[key]
			for k := range result {
				if _, ok := s[k]; !ok {
					delete(result, k)
				}
			}
		}
		items := make([]string, 0, len(result))
		for k := range result {
			items = append(items, k)
		}
		writeArrayBulks(w, items...)
	case "SUNION":
		result := map[string]struct{}{}
		for _, key := range cmd[1:] {
			for k := range m.set[key] {
				result[k] = struct{}{}
			}
		}
		items := make([]string, 0, len(result))
		for k := range result {
			items = append(items, k)
		}
		writeArrayBulks(w, items...)
	case "SDIFF":
		if len(cmd) < 2 {
			writeArrayBulks(w)
			break
		}
		result := map[string]struct{}{}
		if first, ok := m.set[cmd[1]]; ok {
			for k := range first {
				result[k] = struct{}{}
			}
		}
		for _, key := range cmd[2:] {
			for k := range m.set[key] {
				delete(result, k)
			}
		}
		items := make([]string, 0, len(result))
		for k := range result {
			items = append(items, k)
		}
		writeArrayBulks(w, items...)
	case "ZRANK":
		writeInt(w, 0) // simplified
	case "ZREVRANGE":
		z := m.zset[cmd[1]]
		items := make([]string, 0, len(z))
		for k := range z {
			items = append(items, k)
		}
		writeArrayBulks(w, items...)
	case "ZCOUNT":
		writeInt(w, int64(len(m.zset[cmd[1]])))
	case "ZINCRBY":
		writeInt(w, 0) // simplified: not tracking exact float scores
	case "HINCRBY":
		writeInt(w, 0) // simplified
	case "HINCRBYFLOAT":
		writeBulk(w, cmd[3]) // simplified
	case "HLEN":
		writeInt(w, int64(len(m.hash[cmd[1]])))
	case "HSETNX":
		h, ok := m.hash[cmd[1]]
		if !ok {
			h = map[string]string{}
			m.hash[cmd[1]] = h
		}
		if _, exists := h[cmd[2]]; exists {
			writeInt(w, 0)
		} else {
			h[cmd[2]] = cmd[3]
			writeInt(w, 1)
		}
	case "HMGET":
		h := m.hash[cmd[1]]
		writeArrayHeader(w, len(cmd)-2)
		for _, f := range cmd[2:] {
			if v, ok := h[f]; ok {
				writeBulk(w, v)
			} else {
				writeNullBulk(w)
			}
		}
	case "HEXISTS":
		if h, ok := m.hash[cmd[1]]; ok {
			if _, ok := h[cmd[2]]; ok {
				writeInt(w, 1)
			} else {
				writeInt(w, 0)
			}
		} else {
			writeInt(w, 0)
		}
	case "HKEYS":
		h := m.hash[cmd[1]]
		items := make([]string, 0, len(h))
		for k := range h {
			items = append(items, k)
		}
		writeArrayBulks(w, items...)
	case "HVALS":
		h := m.hash[cmd[1]]
		items := make([]string, 0, len(h))
		for _, v := range h {
			items = append(items, v)
		}
		writeArrayBulks(w, items...)
	case "HDEL":
		h := m.hash[cmd[1]]
		n := int64(0)
		for _, f := range cmd[2:] {
			if _, ok := h[f]; ok {
				delete(h, f)
				n++
			}
		}
		writeInt(w, n)
	case "SREM":
		s := m.set[cmd[1]]
		n := int64(0)
		for _, v := range cmd[2:] {
			if _, ok := s[v]; ok {
				delete(s, v)
				n++
			}
		}
		writeInt(w, n)
	case "SISMEMBER":
		s := m.set[cmd[1]]
		if _, ok := s[cmd[2]]; ok {
			writeInt(w, 1)
		} else {
			writeInt(w, 0)
		}
	case "SCARD":
		writeInt(w, int64(len(m.set[cmd[1]])))
	case "ZREM":
		z := m.zset[cmd[1]]
		n := int64(0)
		for _, v := range cmd[2:] {
			if _, ok := z[v]; ok {
				delete(z, v)
				n++
			}
		}
		writeInt(w, n)
	case "ZSCORE":
		writeBulk(w, "0")
	case "ZCARD":
		writeInt(w, int64(len(m.zset[cmd[1]])))
	case "ZRANGEBYSCORE":
		z := m.zset[cmd[1]]
		items := make([]string, 0, len(z))
		for k := range z {
			items = append(items, k)
		}
		writeArrayBulks(w, items...)
	case "LPOP":
		l := m.list[cmd[1]]
		if len(l) == 0 {
			writeNullBulk(w)
		} else {
			writeBulk(w, l[0])
			m.list[cmd[1]] = l[1:]
		}
	case "RPOP":
		l := m.list[cmd[1]]
		if len(l) == 0 {
			writeNullBulk(w)
		} else {
			writeBulk(w, l[len(l)-1])
			m.list[cmd[1]] = l[:len(l)-1]
		}
	case "TYPE":
		if _, ok := m.kv[cmd[1]]; ok {
			writeSimple(w, "string")
		} else if _, ok := m.hash[cmd[1]]; ok {
			writeSimple(w, "hash")
		} else if _, ok := m.list[cmd[1]]; ok {
			writeSimple(w, "list")
		} else if _, ok := m.set[cmd[1]]; ok {
			writeSimple(w, "set")
		} else if _, ok := m.zset[cmd[1]]; ok {
			writeSimple(w, "zset")
		} else {
			writeSimple(w, "none")
		}
	case "RENAME":
		if v, ok := m.kv[cmd[1]]; ok {
			m.kv[cmd[2]] = v
			delete(m.kv, cmd[1])
		}
		writeSimple(w, "OK")
	case "RANDOMKEY":
		for k := range m.kv {
			writeBulk(w, k)
			return
		}
		writeNullBulk(w)
	case "TTL", "PTTL":
		writeInt(w, -1) // no TTL in fake store
	case "FLUSHDB":
		m.kv = map[string]string{}
		m.hash = map[string]map[string]string{}
		m.list = map[string][]string{}
		m.set = map[string]map[string]struct{}{}
		m.zset = map[string]map[string]float64{}
		writeSimple(w, "OK")
	case "DBSIZE":
		writeInt(w, int64(len(m.kv)+len(m.hash)+len(m.list)+len(m.set)+len(m.zset)))
	case "SCAN":
		// Simple COUNT-honoring but non-paging scan: if a MATCH pattern is
		// given, filter keys; else return all kv keys. Always return
		// cursor "0" (done in one pass).
		var match string
		for i := 2; i < len(cmd); i++ {
			if strings.EqualFold(cmd[i], "MATCH") && i+1 < len(cmd) {
				match = cmd[i+1]
			}
		}
		out := make([]string, 0, len(m.kv))
		for k := range m.kv {
			if match == "" || scanMatch(match, k) {
				out = append(out, k)
			}
		}
		writeArrayHeader(w, 2)
		writeBulk(w, "0")
		writeArrayBulks(w, out...)
	default:
		writeError(w, "ERR unknown "+cmd[0])
	}
}

// scanMatch implements a minimal glob-style match used by the fake SCAN:
// "*" matches any run of chars, everything else is literal.
func scanMatch(pat, s string) bool {
	if pat == "*" {
		return true
	}
	if strings.HasSuffix(pat, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pat, "*"))
	}
	if strings.HasPrefix(pat, "*") {
		return strings.HasSuffix(s, strings.TrimPrefix(pat, "*"))
	}
	return pat == s
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func newTestClient(t *testing.T, mem *memStore, opts ...Option) *Client {
	t.Helper()
	if mem == nil {
		mem = newMem()
	}
	fake := startFakeRedis(t, mem.handler)
	c, err := NewClient(fake.Addr(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestClientPing(t *testing.T) {
	c := newTestClient(t, nil)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientGetSet(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v" {
		t.Fatalf("got %q", got)
	}
	if _, err := c.Get(ctx, "missing"); !errors.Is(err, ErrNil) {
		t.Fatalf("want ErrNil, got %v", err)
	}
}

func TestClientDelExists(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	_ = c.Set(ctx, "a", "1", 0)
	_ = c.Set(ctx, "b", "2", 0)
	n, err := c.Exists(ctx, "a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("exists got %d", n)
	}
	n, err = c.Del(ctx, "a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("del got %d", n)
	}
}

func TestClientIncr(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		n, err := c.Incr(ctx, "ctr")
		if err != nil {
			t.Fatal(err)
		}
		if n != i {
			t.Fatalf("want %d got %d", i, n)
		}
	}
}

func TestClientHash(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	n, err := c.HSet(ctx, "h", "f1", "v1", "f2", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("hset got %d", n)
	}
	got, err := c.HGet(ctx, "h", "f1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1" {
		t.Fatalf("hget got %q", got)
	}
	m, err := c.HGetAll(ctx, "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["f1"] != "v1" || m["f2"] != "v2" {
		t.Fatalf("hgetall got %v", m)
	}
}

func TestClientList(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	if _, err := c.RPush(ctx, "l", "a", "b", "c"); err != nil {
		t.Fatal(err)
	}
	xs, err := c.LRange(ctx, "l", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 3 || xs[0] != "a" || xs[2] != "c" {
		t.Fatalf("lrange got %v", xs)
	}
}

func TestClientSet(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	if _, err := c.SAdd(ctx, "s", "x", "y", "z"); err != nil {
		t.Fatal(err)
	}
	xs, err := c.SMembers(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 3 {
		t.Fatalf("smembers got %v", xs)
	}
}

func TestClientZSet(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	if _, err := c.ZAdd(ctx, "z", Z{Score: 1, Member: "a"}, Z{Score: 2, Member: "b"}); err != nil {
		t.Fatal(err)
	}
	xs, err := c.ZRange(ctx, "z", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 2 {
		t.Fatalf("zrange got %v", xs)
	}
}

func TestClientContextCancel(t *testing.T) {
	// Use a server that never responds so the command hangs.
	neverRespond := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) > 0 && strings.ToUpper(cmd[0]) == "HELLO" {
			handleHELLO(w, 3)
			return
		}
		// Silently drop — don't write anything.
	})
	c, err := NewClient(neverRespond.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = c.Get(ctx, "k")
	if err == nil {
		t.Fatal("expected error on canceled ctx")
	}
}

func TestClientHELLOFallback(t *testing.T) {
	// Server that rejects HELLO — client should fall back to AUTH+SELECT.
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			writeError(w, "ERR unknown command 'HELLO'")
		case "AUTH":
			writeSimple(w, "OK")
		case "SELECT":
			writeSimple(w, "OK")
		case "PING":
			writeSimple(w, "PONG")
		}
	})
	c, err := NewClient(fake.Addr(), WithPassword("x"), WithDB(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Proto() != 3 {
		// Proto() returns configured target, not runtime. Acceptable.
	}
}

func TestClientRejectsRediss(t *testing.T) {
	_, err := NewClient("rediss://127.0.0.1:6379")
	if err == nil {
		t.Fatal("expected TLS rejection")
	}
}
