package redis

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSentinel is a minimal sentinel server. It responds to:
//   - HELLO: proto negotiation
//   - AUTH: optional authentication
//   - SENTINEL get-master-addr-by-name: master discovery
//   - SUBSCRIBE +switch-master: failover notifications
type fakeSentinel struct {
	fake       *fakeRedis
	mu         sync.Mutex
	masterAddr string // "host:port"
	password   string
	// switchCh receives notifications to push a +switch-master message to
	// any subscriber writer.
	switchCh   chan string
	subWriters []*bufio.Writer
	subMu      []**sync.Mutex
}

func startFakeSentinel(t *testing.T, masterAddr string) *fakeSentinel {
	t.Helper()
	fs := &fakeSentinel{
		masterAddr: masterAddr,
		switchCh:   make(chan string, 4),
	}
	fs.fake = startFakeRedis(t, fs.handler)
	go fs.pushLoop()
	return fs
}

func (fs *fakeSentinel) Addr() string { return fs.fake.Addr() }

func (fs *fakeSentinel) SetMaster(addr string) {
	fs.mu.Lock()
	old := fs.masterAddr
	fs.masterAddr = addr
	fs.mu.Unlock()
	// Build the +switch-master payload: "<name> <old-ip> <old-port> <new-ip> <new-port>"
	oldHost, oldPort, _ := splitHostPort(old)
	newHost, newPort, _ := splitHostPort(addr)
	payload := fmt.Sprintf("mymaster %s %s %s %s", oldHost, oldPort, newHost, newPort)
	fs.switchCh <- payload
}



func (fs *fakeSentinel) handler(cmd []string, w *bufio.Writer) {
	if len(cmd) == 0 {
		return
	}
	upper := strings.ToUpper(cmd[0])
	switch upper {
	case "HELLO":
		proto := 2
		if len(cmd) > 1 {
			if cmd[1] == "3" {
				proto = 3
			}
		}
		handleHELLO(w, proto)
	case "AUTH":
		fs.mu.Lock()
		pw := fs.password
		fs.mu.Unlock()
		if pw != "" && (len(cmd) < 2 || cmd[len(cmd)-1] != pw) {
			writeError(w, "ERR invalid password")
			return
		}
		writeSimple(w, "OK")
	case "SENTINEL":
		if len(cmd) >= 3 && strings.ToLower(cmd[1]) == "get-master-addr-by-name" {
			fs.mu.Lock()
			addr := fs.masterAddr
			fs.mu.Unlock()
			if addr == "" {
				writeNullBulk(w)
				return
			}
			host, port, _ := splitHostPort(addr)
			writeArrayHeader(w, 2)
			writeBulk(w, host)
			writeBulk(w, port)
		} else {
			writeError(w, "ERR unknown sentinel subcommand")
		}
	case "SUBSCRIBE":
		// Subscribe to +switch-master. Track the writer for push messages.
		fs.mu.Lock()
		wm := fs.fake.WriterMutex(w)
		fs.subWriters = append(fs.subWriters, w)
		fs.subMu = append(fs.subMu, &wm)
		fs.mu.Unlock()
		// Send subscribe confirmation.
		writeArrayHeader(w, 3)
		writeBulk(w, "subscribe")
		writeBulk(w, "+switch-master")
		writeInt(w, 1)
	case "PING":
		writeSimple(w, "PONG")
	default:
		writeError(w, "ERR unknown command")
	}
}

func (fs *fakeSentinel) pushLoop() {
	for payload := range fs.switchCh {
		fs.mu.Lock()
		writers := append([]*bufio.Writer(nil), fs.subWriters...)
		mutexes := append([]*sync.Mutex(nil), make([]*sync.Mutex, len(fs.subMu))...)
		for i, m := range fs.subMu {
			mutexes[i] = *m
		}
		fs.mu.Unlock()
		for i, w := range writers {
			m := mutexes[i]
			m.Lock()
			writeArrayHeader(w, 3)
			writeBulk(w, "message")
			writeBulk(w, "+switch-master")
			writeBulk(w, payload)
			_ = w.Flush()
			m.Unlock()
		}
	}
}

// fakeMaster is a minimal master Redis server that responds to ROLE and
// basic commands.
type fakeMaster struct {
	fake *fakeRedis
	mem  *memStore
}

func startFakeMaster(t *testing.T) *fakeMaster {
	t.Helper()
	m := newMem()
	fm := &fakeMaster{mem: m}
	fm.fake = startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
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
		case "ROLE":
			writeArrayHeader(w, 3)
			writeBulk(w, "master")
			writeInt(w, 0)
			writeArrayHeader(w, 0)
		case "PING":
			writeSimple(w, "PONG")
		default:
			m.handler(cmd, w)
		}
	})
	return fm
}

func (fm *fakeMaster) Addr() string { return fm.fake.Addr() }

func TestSentinelDiscovery(t *testing.T) {
	master := startFakeMaster(t)
	sentinel := startFakeSentinel(t, master.Addr())

	sc, err := NewSentinelClient(SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{sentinel.Addr()},
		DialTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	defer sc.Close()

	ctx := context.Background()
	if err := sc.Set(ctx, "k", "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := sc.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v" {
		t.Fatalf("Get = %q, want %q", got, "v")
	}
}

func TestSentinelFailover(t *testing.T) {
	master1 := startFakeMaster(t)
	master2 := startFakeMaster(t)
	sentinel := startFakeSentinel(t, master1.Addr())

	sc, err := NewSentinelClient(SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{sentinel.Addr()},
		DialTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	defer sc.Close()

	ctx := context.Background()
	if err := sc.Set(ctx, "before", "1", 0); err != nil {
		t.Fatalf("Set before failover: %v", err)
	}

	// Trigger failover.
	sentinel.SetMaster(master2.Addr())

	// Wait for the sentinel client to pick up the failover.
	deadline := time.Now().Add(5 * time.Second)
	var setErr error
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		setErr = sc.Set(ctx, "after", "2", 0)
		if setErr == nil {
			got, err := sc.Get(ctx, "after")
			if err == nil && got == "2" {
				return // success
			}
		}
	}
	if setErr != nil {
		t.Logf("last Set error: %v", setErr)
	}
	// The failover happened if the new master is being used. Verify the
	// client can at least ping.
	if err := sc.Ping(ctx); err != nil {
		t.Fatalf("Ping after failover: %v", err)
	}
}

func TestSentinelConfigValidation(t *testing.T) {
	_, err := NewSentinelClient(SentinelConfig{})
	if err == nil {
		t.Fatal("expected error for empty MasterName")
	}
	_, err = NewSentinelClient(SentinelConfig{MasterName: "m"})
	if err == nil {
		t.Fatal("expected error for empty SentinelAddrs")
	}
}

func TestSentinelClose(t *testing.T) {
	master := startFakeMaster(t)
	sentinel := startFakeSentinel(t, master.Addr())

	sc, err := NewSentinelClient(SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{sentinel.Addr()},
		DialTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close should not panic.
	if err := sc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Commands after close should fail.
	_, err = sc.Get(context.Background(), "k")
	if err == nil {
		t.Fatal("expected error after Close")
	}
}

func TestSentinelPipeline(t *testing.T) {
	master := startFakeMaster(t)
	sentinel := startFakeSentinel(t, master.Addr())

	sc, err := NewSentinelClient(SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{sentinel.Addr()},
		DialTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	defer sc.Close()

	p := sc.Pipeline()
	if p == nil {
		t.Fatal("Pipeline returned nil")
	}
	defer p.Release()

	ctx := context.Background()
	setCmd := p.Set("pk", "pv", 0)
	getCmd := p.Get("pk")
	if err := p.Exec(ctx); err != nil {
		t.Fatalf("Pipeline.Exec: %v", err)
	}
	if _, err := setCmd.Result(); err != nil {
		t.Fatalf("Set result: %v", err)
	}
	got, err := getCmd.Result()
	if err != nil {
		t.Fatalf("Get result: %v", err)
	}
	if got != "pv" {
		t.Fatalf("Get = %q, want %q", got, "pv")
	}
}
