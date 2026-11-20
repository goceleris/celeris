package celeris

import (
	"context"
	"runtime/debug"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// These tests change the PROCESS-GLOBAL GC target, so none of them runs in
// parallel, and each puts the value back (restoreGC) however it ends.

// restoreGC records the current GC target and puts it back when the test ends.
func restoreGC(t *testing.T) int {
	t.Helper()
	orig := debug.SetGCPercent(100)
	debug.SetGCPercent(orig)
	t.Cleanup(func() { debug.SetGCPercent(orig) })
	return orig
}

// currentGC reads the process GC target WITHOUT writing it (a write-and-restore
// read would race with a concurrent apply).
func currentGC() int {
	cur, ok := gcPercentNow()
	if !ok {
		panic("runtime/metrics sample /gc/gogc:percent is not available")
	}
	return cur
}

func TestGCPercentWiring756(t *testing.T) {
	if rc := (Config{GCPercent: 400}).toResourceConfig(); rc.Resources.GCPercent != 400 {
		t.Fatalf("GCPercent not wired: got %d, want 400", rc.Resources.GCPercent)
	}
	if rc := (Config{}).toResourceConfig(); rc.Resources.GCPercent != 0 {
		t.Fatalf("unset GCPercent must map to 0, got %d", rc.Resources.GCPercent)
	}
	// A negative value must reach Validate, not be dropped by a guard.
	rc := (Config{GCPercent: -1}).toResourceConfig().WithDefaults()
	if rc.Resources.GCPercent != -1 {
		t.Fatalf("negative GCPercent must reach validation, got %d", rc.Resources.GCPercent)
	}
	if errs := rc.Validate(); len(errs) == 0 {
		t.Fatal("GCPercent -1 must fail validation (it would switch the collector off)")
	}
}

func TestGCPercentValidation756(t *testing.T) {
	for _, pct := range []int{0, 1, 100, 400, 1000} {
		rc := (Config{GCPercent: pct}).toResourceConfig().WithDefaults()
		for _, err := range rc.Validate() {
			t.Errorf("GCPercent %d: unexpected validation error: %v", pct, err)
		}
	}
	for _, pct := range []int{-1, -100} {
		rc := (Config{GCPercent: pct}).toResourceConfig().WithDefaults()
		if errs := rc.Validate(); len(errs) == 0 {
			t.Errorf("GCPercent %d: want a validation error", pct)
		}
	}
}

func TestGCPercentDefaultOffLeavesRuntime756(t *testing.T) {
	orig := restoreGC(t)
	debug.SetGCPercent(237) // an embedder's own setting
	var g gcPercentState
	g.apply(0)
	if got := currentGC(); got != 237 {
		t.Fatalf("GCPercent 0 changed the process GC target: got %d, want 237", got)
	}
	g.restore()
	if got := currentGC(); got != 237 {
		t.Fatalf("restore with nothing applied changed the target: got %d, want 237", got)
	}
	_ = orig
}

func TestGCPercentApplyRestore756(t *testing.T) {
	restoreGC(t)
	debug.SetGCPercent(150)
	var g gcPercentState
	g.apply(400)
	if got := currentGC(); got != 400 {
		t.Fatalf("after apply: got %d, want 400", got)
	}
	g.restore()
	if got := currentGC(); got != 150 {
		t.Fatalf("after restore: got %d, want the previous 150", got)
	}
	g.restore() // idempotent
	if got := currentGC(); got != 150 {
		t.Fatalf("second restore changed the target: got %d, want 150", got)
	}
}

// The option beats the GOGC environment variable: the runtime reads GOGC once
// at init, so a value that came from it is just the "previous" target here.
func TestGCPercentOverridesEnvValue756(t *testing.T) {
	restoreGC(t)
	debug.SetGCPercent(50) // what GOGC=50 would have left in force
	var g gcPercentState
	g.apply(400)
	if got := currentGC(); got != 400 {
		t.Fatalf("the option must win over the GOGC value: got %d, want 400", got)
	}
	g.restore()
	if got := currentGC(); got != 50 {
		t.Fatalf("restore must bring back the GOGC value: got %d, want 50", got)
	}
}

// Last writer wins: when another writer changed the target after apply, restore
// must leave that value alone.
func TestGCPercentRestoreKeepsForeignValue756(t *testing.T) {
	restoreGC(t)
	debug.SetGCPercent(100)
	var g gcPercentState
	g.apply(400)
	debug.SetGCPercent(250) // another writer
	g.restore()
	if got := currentGC(); got != 250 {
		t.Fatalf("restore overwrote another writer's value: got %d, want 250", got)
	}
}

func TestGCPercentTwoServers756(t *testing.T) {
	restoreGC(t)
	debug.SetGCPercent(100)
	var a, b gcPercentState
	a.apply(400)
	b.apply(300) // last writer wins
	if got := currentGC(); got != 300 {
		t.Fatalf("two servers: got %d, want 300", got)
	}
	a.restore() // the value is not a's any more: left alone
	if got := currentGC(); got != 300 {
		t.Fatalf("first server's shutdown must keep the later value: got %d, want 300", got)
	}
	b.restore() // b's previous value was a's 400
	if got := currentGC(); got != 400 {
		t.Fatalf("second server's shutdown puts back what it replaced: got %d, want 400", got)
	}
}

// A second apply without a restore keeps the first previous value.
func TestGCPercentApplyTwiceKeepsFirstPrev756(t *testing.T) {
	restoreGC(t)
	debug.SetGCPercent(100)
	var g gcPercentState
	g.apply(400)
	g.apply(500)
	if got := currentGC(); got != 400 {
		t.Fatalf("second apply must be a no-op: got %d, want 400", got)
	}
	g.restore()
	if got := currentGC(); got != 100 {
		t.Fatalf("restore: got %d, want 100", got)
	}
}

// End to end through a real Server: Start applies the option, Shutdown puts the
// previous value back. The test uses a prebound-free address on loopback.
func TestGCPercentServerLifecycle756(t *testing.T) {
	restoreGC(t)
	debug.SetGCPercent(100)
	s := New(Config{Addr: "127.0.0.1:0", Engine: Std, GCPercent: 400})
	s.GET("/", func(c *Context) error { return c.String(200, "ok") })
	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()

	deadline := time.Now().Add(5 * time.Second)
	for currentGC() != 400 {
		if time.Now().After(deadline) {
			t.Fatalf("Start did not apply GCPercent: target is %d", currentGC())
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	<-errc
	if got := currentGC(); got != 100 {
		t.Fatalf("Shutdown must put the previous GC target back: got %d, want 100", got)
	}
}

// A server that refuses to start on a bad config must not touch the GC.
func TestGCPercentNegativeRefusedAtStart756(t *testing.T) {
	restoreGC(t)
	debug.SetGCPercent(100)
	s := New(Config{Addr: "127.0.0.1:0", Engine: Std, GCPercent: -1})
	if err := s.Start(); err == nil {
		t.Fatal("Start with GCPercent -1 must fail")
	}
	if got := currentGC(); got != 100 {
		t.Fatalf("a refused start changed the GC target: got %d, want 100", got)
	}
}

func TestGCPercentResourcesZeroValue756(t *testing.T) {
	if got := (resource.Resources{}).GCPercent; got != 0 {
		t.Fatalf("Resources zero value GCPercent = %d, want 0 (off)", got)
	}
}

// A start that fails to bind has no Shutdown to come, yet the GC target must
// still be back where it was once Start returns.
func TestGCPercentRestoredWhenListenFails756(t *testing.T) {
	restoreGC(t)
	debug.SetGCPercent(100)
	hold := New(Config{Addr: "127.0.0.1:0", Engine: Std})
	hold.GET("/", func(c *Context) error { return c.String(200, "ok") })
	go func() { _ = hold.Start() }()
	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for addr == "" {
		if time.Now().After(deadline) {
			t.Fatal("holder server did not start")
		}
		if a := hold.Addr(); a != nil {
			addr = a.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hold.Shutdown(ctx)
	}()
	s := New(Config{Addr: addr, Engine: Std, GCPercent: 400})
	if err := s.Start(); err == nil {
		t.Fatal("Start on an address already in use must fail")
	}
	if got := currentGC(); got != 100 {
		t.Fatalf("a failed Start must put the GC target back without a Shutdown: got %d, want 100", got)
	}
}
