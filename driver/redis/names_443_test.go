package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/engine"
)

// The engine sentinels the event loop returns are reachable as redis.Err*
// (celeris#443): a caller can match them with errors.Is without importing
// the engine package.
func TestEngineSentinelsNamed443(t *testing.T) {
	if ErrQueueFull != engine.ErrQueueFull || ErrUnknownFD != engine.ErrUnknownFD {
		t.Fatal("ErrQueueFull/ErrUnknownFD are not the engine's sentinels")
	}
	if !errors.Is(fmt.Errorf("write: %w", engine.ErrQueueFull), ErrQueueFull) {
		t.Fatal("a wrapped engine.ErrQueueFull does not match ErrQueueFull")
	}

	// A real event loop: Write on a descriptor the worker never registered.
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	err = prov.WorkerLoop(0).Write(1<<30, []byte("PING\r\n"))
	if !errors.Is(err, ErrUnknownFD) {
		t.Fatalf("Write on an unregistered fd: err = %v, want ErrUnknownFD", err)
	}
	if errors.Is(err, ErrQueueFull) {
		t.Fatalf("Write on an unregistered fd: err = %v matches ErrQueueFull too", err)
	}
}

// The codec's oversize errors reach the caller unwrapped, so they are
// named here (celeris#443).
func TestCodecOversizeErrorsReachCaller443(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  error
	}{
		{"bulk", "$600000000\r\n", ErrProtocolOversizedBulk},
		{"array", "*200000000\r\n", ErrProtocolOversizedArray},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := newMem()
			fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
				if strings.EqualFold(cmd[0], "GET") {
					_, _ = w.WriteString(tc.reply)
					return
				}
				mem.handler(cmd, w)
			})
			c, err := NewClient(fake.Addr())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = c.Get(ctx, "k")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Get: err = %v, want %v", err, tc.want)
			}
		})
	}
}

// The aliases name the codec's types: a Value built with the redis names
// is the codec's Value.
func TestValueNames443(t *testing.T) {
	v := Value{Type: TyMap, Map: []KV{{K: Value{Type: TySimple, Str: []byte("k")}, V: Value{Type: TyInt, Int: 7}}}}
	typeName := func(ty Type) string { return ty.String() }
	if got := typeName(v.Map[0].V.Type); got != "int" {
		t.Fatalf("Type = %v", got)
	}
	if len(PoolStats{PerWorker: []PoolWorkerStats{{Idle: 1}}}.PerWorker) != 1 {
		t.Fatal("PoolStats")
	}
}
