//go:build linux

package epoll

import (
	"net"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/tcpack"
)

// celeris#1404: the host probe (internal/tcpack) binds an ephemeral loopback
// port, so it must not run while a caller's own port is still being bound. The
// engine starts it only after every loop has bound its listener, not in New.
// A collision between the probe's bind(0) and a port the caller has just freed
// cannot be made to happen on demand, so the test judges the order: with port 0
// (the only path on which portHoldHook runs) the probe has not been started when
// Listen decides the port, nor when a loop has bound its listener, and has been
// by the time Listen returns. The probe is started once per process, so the test
// runs in a process of its own.
func TestTcpackProbeStartsAfterTheLoopsBind1404(t *testing.T) {
	if !child848(t) {
		return
	}
	if tcpack.Started() {
		t.Fatal("the probe was already started in a fresh process, before the engine was built")
	}
	var hookRan, startedAtHook, bound, startedAtBind atomic.Bool
	portHoldHook = func(string) {
		hookRan.Store(true)
		startedAtHook.Store(tcpack.Started())
	}
	t.Cleanup(func() { portHoldHook = nil })
	origAddr := listenAddrOf
	listenAddrOf = func(fd int) net.Addr {
		bound.Store(true)
		if tcpack.Started() {
			startedAtBind.Store(true)
		}
		return origAddr(fd)
	}
	t.Cleanup(func() { listenAddrOf = origAddr })

	r := start895(t, "127.0.0.1:0", 2, false)
	if !hookRan.Load() || !bound.Load() {
		t.Fatalf("the test's hooks did not run (port hold %v, loop bound %v)", hookRan.Load(), bound.Load())
	}
	if startedAtHook.Load() {
		t.Fatal("the probe had been started before Listen decided the port: its bind(0) runs beside the caller's own setup")
	}
	if startedAtBind.Load() {
		t.Fatal("the probe had been started when a loop bound its listener: it must wait until every loop has bound")
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if !tcpack.Started() {
		t.Fatal("Listen returned and the probe was never started: the h2 idle clock would wait for its lazy start")
	}
}
