//go:build linux && validation

package iouring

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/recvtheft"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#685, the hijack path. hijackConn hands the socket to the handler as
// a net.Conn and queues the cancel of any op the worker still has on it; the
// cancel reaches the kernel at the worker's next io_uring_enter. An op the
// kernel still owes the connection there can read the hijacker's first bytes
// first. Which op, and on which ring:
//
//   - single-shot recv (the default): the recv that brought the request has
//     completed before its handler runs, so nothing is owed at the hijack
//     (TestRecvTheft685HijackSingleShot: the witness stays 0);
//   - multishot recv (CELERIS_IOURING_MULTISHOT_RECV=1): the recv stays
//     armed across its request, so it is owed. Its completion runs as task
//     work. On a DEFER_TASKRUN ring that work runs only inside the enter,
//     after the submit of the queued cancel, so the cancel wins
//     (TestRecvTheft685HijackMultishotDefer). On a ring without it (kernels
//     before 6.1, or wherever the probe finds DEFER_TASKRUN unusable) it runs
//     at the worker thread's next return from any syscall, before that enter
//     (TestRecvTheft685HijackMultishotCoop, a COOP_TASKRUN ring as the high
//     tier builds without DEFER_TASKRUN).
//
// Each test hijacks one connection with the worker held right after the
// hijack (recvtheft.SetHijackHold), has the client send a payload during the
// hold, and asserts that the hijacker reads it and that no stale recv read
// it. Needs CELERIS_RECV_THEFT_715=1 and an io_uring worker (one is enough).

const (
	hijack685Hold    = 300 * time.Millisecond
	hijack685Payload = "PING-685-hijacker-first-bytes\n"
)

type hijack685Handler struct{ conns chan net.Conn }

func (h hijack685Handler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	if s.Path != "/hijack" {
		return s.ResponseWriter.WriteResponse(s, 200,
			[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
	}
	hj, ok := s.ResponseWriter.(stream.Hijacker)
	if !ok {
		h.conns <- nil
		return nil
	}
	c, err := hj.Hijack(s)
	if err != nil {
		h.conns <- nil
		return nil
	}
	h.conns <- c
	return nil
}

// hijack685Tier is how a test arm sets the engine's tier up before Listen.
type hijack685Tier int

const (
	hijackTierDefault    hijack685Tier = iota // as probed; single-shot recv
	hijackTierMultiDefer                      // multishot recv, DEFER_TASKRUN ring
	hijackTierMultiCoop                       // multishot recv, COOP_TASKRUN ring
)

func runHijack685(t *testing.T, tier hijack685Tier) {
	t.Helper()
	requireRecvTheft715(t)
	if tier != hijackTierDefault {
		t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", "1")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	h := hijack685Handler{conns: make(chan net.Conn, 1)}
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, h)
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	if tier != hijackTierDefault {
		ht, ok := e.tier.(*highTier)
		if !ok || !ht.multishotRecv {
			skipOrFail656(t, "celeris#685 hijack arm needs the high tier with multishot recv; have %T", e.tier)
		}
		if tier == hijackTierMultiDefer && !ht.deferTaskrun {
			skipOrFail656(t, "celeris#685 hijack arm needs DEFER_TASKRUN")
		}
		cp := *ht
		cp.deferTaskrun = tier == hijackTierMultiDefer
		cp.fixedFiles = false
		e.tier = &cp
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	}()
	for deadline := time.Now().Add(8 * time.Second); e.Addr() == nil; {
		select {
		case err := <-done:
			skipOrFail656(t, "iouring engine failed to start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not start listening within 8s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var multishot, deferTR bool
	for _, w := range e.workers {
		multishot = w.bufRing != nil
		deferTR = w.tier.SetupFlags()&setupDeferTaskrun != 0
	}
	if (tier != hijackTierDefault) != multishot {
		skipOrFail656(t, "celeris#685 hijack arm: multishot recv is %v on the worker, want %v", multishot, tier != hijackTierDefault)
	}

	recvtheft.SetHijackHold(hijack685Hold)
	defer recvtheft.SetHijackHold(0)
	holds0 := recvtheft.HijackHolds()
	owed0 := recvtheft.HijackWithOpOwed()
	stale0 := e.metrics.handoffLoss.staleRecvDataClosed.Load()

	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("GET /hijack HTTP/1.1\r\nHost: recv-theft-685\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// The payload goes out while the worker is held after the hijack, before
	// its next enter.
	for deadline := time.Now().Add(2 * time.Second); recvtheft.HijackHolds() == holds0; {
		if time.Now().After(deadline) {
			t.Fatal("the hijack hold never ran: the request did not reach Hijack")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := c.Write([]byte(hijack685Payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	var hc net.Conn
	select {
	case hc = <-h.conns:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never returned from Hijack")
	}
	if hc == nil {
		t.Fatal("Hijack failed")
	}
	defer func() { _ = hc.Close() }()
	_ = hc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	var got []byte
	for !strings.Contains(string(got), hijack685Payload) {
		n, err := hc.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	owed := recvtheft.HijackWithOpOwed() - owed0
	stale := e.metrics.handoffLoss.staleRecvDataClosed.Load() - stale0
	read := strings.Contains(string(got), hijack685Payload)
	t.Logf("RECVTHEFT685 hijack result tier=%d multishot=%v defer_taskrun=%v op_owed=+%d stale_recv_data_closed=+%d hijacker_read=%v got=%q",
		tier, multishot, deferTR, owed, stale, read, got)
	if tier == hijackTierDefault && owed != 0 {
		t.Errorf("a single-shot hijack found an op owed (+%d): the recv that brought the request should have completed", owed)
	}
	if tier != hijackTierDefault && owed != 1 {
		t.Errorf("the multishot hijack found no op owed (+%d): the arm did not reach its precondition", owed)
	}
	if !read || stale != 0 {
		t.Fatalf("the hijacker lost its first bytes: read=%v (got %q), stale recv data on the closed identity +%d", read, got, stale)
	}
}

// TestRecvTheft685HijackSingleShot: nothing is owed at a single-shot hijack.
func TestRecvTheft685HijackSingleShot(t *testing.T) { runHijack685(t, hijackTierDefault) }

// TestRecvTheft685HijackMultishotDefer: owed, and the queued cancel still
// wins, because the recv's completion waits for the enter.
func TestRecvTheft685HijackMultishotDefer(t *testing.T) { runHijack685(t, hijackTierMultiDefer) }

// TestRecvTheft685HijackMultishotCoop: owed, and without DEFER_TASKRUN the
// recv's completion runs at the worker's next syscall return, before the
// enter that would submit the cancel queued by hijackConn.
func TestRecvTheft685HijackMultishotCoop(t *testing.T) { runHijack685(t, hijackTierMultiCoop) }
