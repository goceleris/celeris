package redis

import (
	"encoding/json"
	"expvar"
	"testing"
)

// TestDirectWaitExpvar854: the expvar is registered and reads back the mode and
// the snapshot, and reading it does not latch the environment.
func TestDirectWaitExpvar854(t *testing.T) {
	v := expvar.Get("celeris_redis_directwait")
	if v == nil {
		t.Fatal("expvar celeris_redis_directwait is not registered")
	}
	read := func() map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(v.String()), &m); err != nil {
			t.Fatalf("expvar is not JSON: %v: %s", err, v.String())
		}
		return m
	}

	resetDirectWaitForTest(t)
	t.Setenv(directWaitEnv, "auto")
	if m := read(); m["mode"] != "unread" || m["env"] != "auto" {
		t.Fatalf("before first use: mode=%v env=%v, want unread/auto", m["mode"], m["env"])
	}
	if got := currentDirectWait(); got != directWaitAuto {
		t.Fatalf("reading the expvar latched the mode: currentDirectWait = %d, want auto", got)
	}

	setDirectWaitForTest(t, directWaitBlock)
	before := directWaitSnapshot()
	directWaitStats.blocks.Add(3)
	directWaitStats.sliceExpiries.Add(5)
	directWaitStats.handoffs.Add(7)
	directWaitStats.netpollParks.Add(11)
	t.Cleanup(func() {
		directWaitStats.blocks.Add(^uint64(2))
		directWaitStats.sliceExpiries.Add(^uint64(4))
		directWaitStats.handoffs.Add(^uint64(6))
		directWaitStats.netpollParks.Add(^uint64(10))
	})
	m := read()
	want := map[string]uint64{
		"blocks": before.Blocks + 3, "slice_expiries": before.SliceExpiries + 5,
		"handoffs": before.Handoffs + 7, "netpoll_parks": before.NetpollParks + 11,
	}
	for k, w := range want {
		if got, _ := m[k].(float64); uint64(got) != w {
			t.Errorf("%s = %v, want %d", k, m[k], w)
		}
	}
	if m["mode"] != "block" {
		t.Errorf("mode = %v, want block", m["mode"])
	}
	if _, ok := m["block_supported"].(bool); !ok {
		t.Errorf("block_supported missing or not a bool: %v", m["block_supported"])
	}
}
