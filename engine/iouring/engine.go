//go:build linux

package iouring

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// Engine implements the io_uring-based I/O engine.
type Engine struct {
	workers      []*Worker
	tier         TierStrategy
	profile      engine.CapabilityProfile
	cfg          resource.Config
	handler      stream.Handler
	addr         atomic.Pointer[net.Addr]
	mu           sync.Mutex
	acceptPaused atomic.Bool
	// pause records the pause in progress for the workers' linger step
	// (celeris#662): when it began and whether its listeners get the
	// tcp_synack_retries=0 guard. Written by BeginPauseAccept before it sets
	// acceptPaused.
	pause   deferlinger.PauseState
	metrics struct {
		reqCount    atomic.Uint64
		activeConns atomic.Int64
		// errs is the per-cause ErrorCount breakdown (celeris#645).
		// EngineMetrics.ErrorCount is its sum; no separate total exists.
		errs errclass.Counters
		// asyncPromoted counts the cumulative inline → dispatch-goroutine
		// promotions across all workers (celeris #300).
		asyncPromoted atomic.Uint64
		// acceptCount / closeCount track cumulative connection lifecycle
		// events; bytesRead / bytesWritten track cumulative payload bytes.
		// All four feed the adaptive controller's load signals. Bytes are
		// batched per-worker and flushed once per event-loop iteration
		// (mirroring reqCount) to avoid hot-path cache-line bouncing;
		// accepts/closes are infrequent so they increment directly like
		// activeConns.
		acceptCount  atomic.Uint64
		closeCount   atomic.Uint64
		bytesRead    atomic.Uint64
		bytesWritten atomic.Uint64
		// transplantCount / transplantRR support #383 connection transplant:
		// the cumulative number of conns adopted from another engine, and a
		// round-robin cursor for spreading adopts across workers.
		transplantCount atomic.Uint64
		transplantRR    atomic.Uint64
		// transplantDetached / transplantSlotOccupied complete the #383
		// hand-off ledger transplantCount starts (celeris#624):
		// conns detached FOR epoll (reverse direction) and adoptions
		// refused because the conn-table slot was taken. Neither side of a
		// hand-off fires a lifecycle hook, so the pair is the only way to
		// reconcile one from outside the engine.
		transplantDetached     atomic.Uint64
		transplantSlotOccupied atomic.Uint64
		// The remaining silent drop points on the #383 path, one bucket
		// each (celeris#624).
		transplantHandoffRefused atomic.Uint64
		transplantAdoptRefused   atomic.Uint64
		// closeMissingConnState counts finishClose calls that moved the
		// gauge and closeCount for an already-nil connState, which skips
		// OnDisconnect — the silent close path of celeris#624.
		closeMissingConnState atomic.Uint64
		// recvArm holds the recv-arming witnesses (celeris#586).
		recvArm recvArmStats
		// detachedConns mirrors the sum of the workers' private detachedCount
		// (one atomic add per detach and per detached close, none per
		// request); detachWindowCloses counts closes that landed inside the
		// celeris#549 window (Detach published, deferred count not yet
		// taken). Both exist so the accounting can be observed from outside
		// the worker thread (celeris#584).
		detachedConns      atomic.Int64
		detachWindowCloses atomic.Uint64
		// zc holds the SEND_ZC exposure witnesses (celeris#591).
		zc zcStats
		// handoffLoss holds the celeris#657 hand-off loss witnesses: stale
		// recv data by identity class, and hand-offs made with an op in
		// flight.
		handoffLoss handoffLossStats
		// sweep holds the post-switch sweep's witnesses (celeris#657 P9):
		// its pass count, and the live residue by refusal class, which
		// every worker publishes as a delta so the sum is what the engine
		// still holds against a drain.
		sweep sweepCounters
	}
	// asyncRoutes is cached from the handler's HasAsyncRoutes/route count
	// at construction so Metrics() doesn't pay the type-assertion per
	// call. Snapshot-at-Listen — static post-Start.
	asyncRoutes int
	// asyncCancelFlags is whether probeAsyncCancelFlags found this kernel
	// accepting IORING_ASYNC_CANCEL_* flags (5.19+): false when the kernel
	// rejected them, when it answered in a way the probe does not recognise
	// and when the probe got no answer. Every worker gets a copy; the
	// hand-off's REAP needs them (celeris#657).
	asyncCancelFlags bool
}

// New creates a new io_uring engine.
func New(cfg resource.Config, handler stream.Handler) (*Engine, error) {
	cfg = cfg.WithDefaults()
	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("config validation: %w", errs[0])
	}

	profile := probe.Probe()
	if !profile.IOUringTier.Available() {
		return nil, fmt.Errorf("io_uring not available on this system")
	}

	// Runtime feature probes: verify features actually work on this kernel,
	// overriding version-based detection which is unreliable (e.g., AWS
	// kernels may have features disabled or partially broken).
	// SEND_ZC runtime probe with IORING_SEND_ZC_REPORT_USAGE.
	// Distinguishes: unsupported, broken (ENA), copy fallback, true zero-copy.
	if profile.SendZC {
		zcResult, zcReason := probeSendZCCached()
		functional := zcResult == SendZCTrueZeroCopy || zcResult == SendZCCopyFallback
		switch zcResult {
		case SendZCTrueZeroCopy:
			cfg.Logger.Info("SEND_ZC probe result", "functional", true, "loopback", "true zero-copy")
		case SendZCCopyFallback:
			if zcReason != "" {
				cfg.Logger.Info("SEND_ZC probe result", "functional", true, "loopback", "copy-fallback (expected)", "reason", zcReason)
			} else {
				cfg.Logger.Info("SEND_ZC probe result", "functional", true, "loopback", "copy-fallback (expected)")
			}
		default:
			if zcReason != "" {
				cfg.Logger.Info("SEND_ZC probe result", "functional", false, "result", zcResult.String(), "reason", zcReason)
			} else {
				cfg.Logger.Info("SEND_ZC probe result", "functional", false, "result", zcResult.String())
			}
		}
		envVal := os.Getenv("CELERIS_IOURING_SEND_ZC")
		enabled, recognized := resolveSendZCPolicy(functional, envVal)
		if !recognized {
			cfg.Logger.Warn("unrecognized CELERIS_IOURING_SEND_ZC value, falling back to auto", "value", envVal)
		}
		profile.SendZC = enabled
	}
	if profile.FixedFiles {
		if ok, ffReason := probeFixedFilesCached(); !ok {
			cfg.Logger.Info("fixed files runtime probe failed, disabling", "reason", ffReason)
			profile.FixedFiles = false
		}
	}
	// ProvidedBuffers: tested via IORING_REGISTER_PBUF_RING. Without it,
	// multishot recv has no backing store, so MultishotRecv is also forced
	// off when this probe fails.
	if profile.ProvidedBuffers {
		if ok, pbReason := probeProvidedBuffersCached(); !ok {
			cfg.Logger.Info("provided buffers runtime probe failed, disabling (downgrades to base tier and disables multishot recv)", "reason", pbReason)
			profile.ProvidedBuffers = false
			profile.MultishotRecv = false
		}
	}
	// MultishotAccept: kernel may advertise it but never set CQE_F_MORE,
	// leaving accept stuck after the first completion. Probe submits a
	// real multishot accept + dial and verifies the F_MORE flag.
	if profile.MultishotAccept {
		if ok, maReason := probeMultishotAcceptCached(); !ok {
			cfg.Logger.Info("multishot accept runtime probe failed, disabling (worker will use single-shot accept)", "reason", maReason)
			profile.MultishotAccept = false
		}
	}

	// The io_uring→epoll hand-off cancels an armed recv before it moves a
	// connection (REAP, celeris#657), and that cancel needs
	// IORING_ASYNC_CANCEL flags, which exist from Linux 5.19. Unless the
	// probe finds them accepted the hand-off never reaps: a sync connection
	// whose recv is armed stays on io_uring until that recv completes on its
	// own, and its next response is then HELD and handed off with nothing in
	// flight. HOLD needs a worker without a provided-buffer ring. A kernel
	// that rejects the flags has none (buffer rings arrived in the same
	// release, 5.19); where one exists because the probe got no answer on a
	// newer kernel, the connection is not held and stays on io_uring. A
	// promoted async connection is never offered for the hand-off without
	// the flags and stays too. Placement only, either way. Only the kernel's
	// answer is cached: after a probe with no answer (or one it does not
	// recognise) the next New probes again (celeris#681 N1).
	asyncCancel, acReason := probeAsyncCancelFlagsCached()
	asyncCancelFlags := asyncCancel == asyncCancelAccepted
	logAsyncCancelProbe(cfg.Logger, asyncCancel, acReason, profile.KernelMajor, profile.KernelMinor)

	tier := SelectTier(profile, 2*time.Second)
	if tier == nil {
		return nil, fmt.Errorf("no suitable io_uring tier available")
	}

	cfg.Logger.Info("io_uring engine selected",
		"tier", tier.Tier().String(),
		"multishot_accept", tier.SupportsMultishotAccept(),
		"multishot_recv", tier.SupportsMultishotRecv(),
		"provided_buffers", tier.SupportsProvidedBuffers(),
		// The EFFECTIVE value, not the tier capability: this used to print
		// the capability and so reported fixed_files=true while the feature
		// was gated off (celeris#541).
		"fixed_files", fixedFilesEnabled(tier.SupportsFixedFiles()),
		"send_zc", tier.SupportsSendZC(),
		"async_cancel_flags", asyncCancelFlags,
		"async_cancel_probe", asyncCancel.String(),
	)

	e := &Engine{
		tier:             tier,
		profile:          profile,
		cfg:              cfg,
		handler:          handler,
		asyncCancelFlags: asyncCancelFlags,
	}
	// Snapshot the static AsyncRoutes count from the handler so
	// Metrics() doesn't pay a type-assertion per call (#300 G3).
	if r, ok := handler.(interface{ AsyncRouteCount() int }); ok {
		e.asyncRoutes = r.AsyncRouteCount()
	}
	return e, nil
}

// logAsyncCancelProbe reports an async-cancel-flags probe that did not find
// the flags accepted (celeris#681 R2). A rejection is the kernel's answer and
// expected before 5.19: Info. An answer the probe does not recognise is one
// no kernel measured gives, on any version: Warn, with the reason, which
// names the errno (celeris#681 N2). A probe that got no answer says nothing
// about the kernel; on one whose version has the flags (5.19 and later) it is
// unexpected, and it keeps the hand-off's reap off for the engine being built
// (the next New probes again: probeAsyncCancelFlagsCached keeps only an
// answer), so it is a Warn there and Info below.
func logAsyncCancelProbe(l *slog.Logger, p asyncCancelProbe, reason string, kernelMajor, kernelMinor int) {
	kernel := fmt.Sprintf("%d.%d", kernelMajor, kernelMinor)
	switch p {
	case asyncCancelRejected:
		l.Info("async cancel flags rejected by the kernel: the io_uring→epoll hand-off will not cancel an armed recv (celeris#657)",
			"reason", reason, "kernel", kernel)
	case asyncCancelUnexpected:
		l.Warn("async cancel flags probe got an answer it does not recognise: the io_uring→epoll hand-off will not cancel an armed recv (celeris#657)",
			"reason", reason, "kernel", kernel)
	case asyncCancelNoAnswer:
		msg := "async cancel flags probe got no answer from the kernel: the io_uring→epoll hand-off will not cancel an armed recv (celeris#657)"
		if kernelMajor > 5 || (kernelMajor == 5 && kernelMinor >= 19) {
			l.Warn(msg, "reason", reason, "kernel", kernel)
		} else {
			l.Info(msg, "reason", reason, "kernel", kernel)
		}
	}
}

// Listen starts the io_uring engine and blocks until context is canceled.
func (e *Engine) Listen(ctx context.Context) error {
	// If a listener was provided (StartWithListener), use its bound address
	// and close the Go-managed listener so our raw sockets can bind with
	// SO_REUSEPORT. Log the ownership transfer so users see it.
	if e.cfg.Listener != nil {
		e.cfg.Addr = e.cfg.Listener.Addr().String()
		if e.cfg.Logger != nil {
			e.cfg.Logger.Info("iouring: closing supplied listener to rebind via SO_REUSEPORT",
				"addr", e.cfg.Addr)
		}
		_ = e.cfg.Listener.Close()
		e.cfg.Listener = nil
	}

	resolved := e.cfg.Resources.Resolve()

	// Cap workers to fit RLIMIT_MEMLOCK. Each worker locks ~12 MB for its
	// ring + buffers; the systemd / kernel default of 8 MB only fits one.
	// Without this, the engine would error out on the first worker past
	// the limit with ENOMEM; capping silently lets a default-ulimit cloud
	// VM keep running on fewer workers (and logs the cap so the operator
	// can fix the limit if they want full parallelism).
	if capped := capWorkersToMemlock(resolved.Workers, e.cfg.Logger); capped < resolved.Workers {
		resolved.Workers = capped
	}

	cpus := platform.DistributeWorkers(resolved.Workers, e.profile.NumCPU, e.profile.NUMANodes)

	// Probe for the highest working tier by test-creating a ring.
	tier := e.tier
	for {
		testRing, err := NewRing(uint32(resolved.SQERingSize), tier.SetupFlags(), tier.SQPollIdle())
		if err == nil {
			_ = testRing.Close()
			break
		}
		lower := fallbackTier(tier)
		if lower == nil {
			return fmt.Errorf("all io_uring tiers failed, last error: %w", err)
		}
		e.cfg.Logger.Warn("io_uring tier failed, falling back",
			"failed_tier", tier.Tier().String(),
			"fallback_tier", lower.Tier().String(),
			"err", err,
		)
		tier = lower
	}
	workers, err := e.createWorkers(tier, cpus, resolved)
	if err != nil {
		return fmt.Errorf("worker init: %w", err)
	}

	// Inner context allows canceling workers if any fail during init.
	innerCtx, innerCancel := context.WithCancel(ctx)
	defer innerCancel()

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run(innerCtx)
		}()
	}

	// Wait for all workers to finish ring initialization (done inside run()
	// after LockOSThread, required by SINGLE_ISSUER).
	for _, w := range workers {
		if initErr := <-w.ready; initErr != nil {
			// A worker failed to create its ring. Cancel all workers.
			innerCancel()
			wg.Wait()
			return initErr
		}
	}

	// celeris#639: publish an address a worker recorded before it signalled
	// ready, never getsockname on a listenFD the worker may have closed since,
	// and refuse to start rather than publish nil: every caller that polls
	// Addr() reads nil as "not bound yet" and waits.
	var addr net.Addr
	for _, w := range workers {
		if w.listenAddr != nil {
			addr = w.listenAddr
			break
		}
	}
	if len(workers) > 0 && addr == nil {
		innerCancel()
		wg.Wait()
		return fmt.Errorf("io_uring: no worker could report the address it bound for %s", e.cfg.Addr)
	}

	e.mu.Lock()
	e.tier = tier
	e.workers = workers
	e.mu.Unlock()
	if addr != nil {
		e.addr.Store(&addr)
	}

	e.cfg.Logger.Info("io_uring engine listening",
		"addr", e.cfg.Addr,
		"tier", tier.Tier().String(),
		"workers", resolved.Workers,
		"sqpoll", tier.SQPollIdle() > 0,
		"send_zc", tier.SupportsSendZC(),
		"fixed_files", fixedFilesEnabled(tier.SupportsFixedFiles()),
		"numa_nodes", e.profile.NUMANodes,
		"kernel", e.profile.KernelVersion,
	)
	if e.cfg.AsyncHandlers && e.cfg.EnableH2Upgrade {
		e.cfg.Logger.Info(
			"AsyncHandlers + EnableH2Upgrade: async dispatch applies to HTTP/1.1 only; H2 conns still run inline on the worker",
		)
	}

	<-ctx.Done()
	// Workers use SubmitAndWaitTimeout and check ctx.Err() on each iteration,
	// so they will exit within ~100ms of context cancellation.
	wg.Wait()
	return nil
}

func (e *Engine) createWorkers(tier TierStrategy, cpus []int,
	resolved resource.ResolvedResources) ([]*Worker, error) {
	workers := make([]*Worker, len(cpus))
	for i := range workers {
		w, err := newWorker(i, cpus[i], tier, e.handler,
			resolved, e.cfg,
			&e.metrics.reqCount, &e.metrics.activeConns, &e.metrics.errs,
			&e.metrics.asyncPromoted, &e.acceptPaused,
			&e.metrics.acceptCount, &e.metrics.closeCount,
			&e.metrics.bytesRead, &e.metrics.bytesWritten)
		if err != nil {
			// Clean up already-created workers.
			for _, prev := range workers[:i] {
				if prev != nil {
					prev.shutdown()
				}
			}
			return nil, err
		}
		w.transplantCount = &e.metrics.transplantCount // #383 transplant counter
		w.recvArm = &e.metrics.recvArm                 // #586 recv-arming witnesses
		w.detachedConns = &e.metrics.detachedConns
		w.detachWindowCloses = &e.metrics.detachWindowCloses
		w.zc = &e.metrics.zc // #591 SEND_ZC exposure witnesses
		// The rest of the #383 hand-off ledger + the silent-close counter
		// (celeris#624).
		w.transplantDetached = &e.metrics.transplantDetached
		w.transplantSlotOccupied = &e.metrics.transplantSlotOccupied
		w.closeMissingConnState = &e.metrics.closeMissingConnState
		w.transplantHandoffRefused = &e.metrics.transplantHandoffRefused
		w.transplantAdoptRefused = &e.metrics.transplantAdoptRefused
		w.handoffLoss = &e.metrics.handoffLoss // celeris#657 witnesses
		w.sweepCnt = &e.metrics.sweep          // celeris#657 P9 sweep witnesses
		w.asyncCancelFlags = e.asyncCancelFlags
		w.pause = &e.pause // celeris#662 pause linger
		workers[i] = w
	}
	return workers, nil
}

// fallbackTier drops the current tier strategy one rung. Invoked when
// NewRing fails (e.g. the kernel rejected a setup flag we asked for) so
// the engine can re-attempt at a lower capability instead of refusing
// to start.
func fallbackTier(current TierStrategy) TierStrategy {
	switch t := current.(type) {
	case *optionalTier:
		return &highTier{
			deferTaskrun:    t.deferTaskrun,
			fixedFiles:      t.fixedFiles,
			sendZC:          t.sendZC,
			multishotAccept: t.multishotAccept,
			multishotRecv:   t.multishotRecv,
		}
	case *highTier:
		return &baseTier{}
	default:
		return nil
	}
}

// Shutdown is a no-op for the io_uring engine — graceful shutdown is
// driven by context cancellation on Listen's parent context. Workers
// exit their run loops on ctx.Done, drain the responses still queued for
// the ring (Worker.hasPendingSends, celeris#595) and call Worker.shutdown,
// which joins async dispatch goroutines via asyncWG. See epoll engine
// Shutdown for the same rationale.
//
// That parent context is always cancellable: every Server.Start* entry
// point owns one and Server.Shutdown cancels it after the graceful phase.
// Handing Listen a context.Background() is what made Start hang here
// (celeris#595), since this method cannot wake it.
func (e *Engine) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return nil
}

// Metrics returns a snapshot of engine metrics.
//
// The worker count is read under e.mu. Listen starts the worker
// goroutines before it assigns e.workers (it waits for every ring to
// come up first), so a worker already serving a request can reach this
// method through Server.EngineInfo while the slice is still being
// written; the race detector reported exactly that pair in 7 of 16
// io_uring cells of probatorium's first -race matrix run (celeris#578).
func (e *Engine) Metrics() engine.EngineMetrics {
	e.mu.Lock()
	workers := len(e.workers)
	e.mu.Unlock()
	m := engine.EngineMetrics{
		RequestCount:       e.metrics.reqCount.Load(),
		ActiveConnections:  e.metrics.activeConns.Load(),
		AsyncRoutes:        e.asyncRoutes,
		AsyncPromotedConns: e.metrics.asyncPromoted.Load(),
		Workers:            workers,
		AcceptCount:        e.metrics.acceptCount.Load(),
		CloseCount:         e.metrics.closeCount.Load(),
		BytesRead:          e.metrics.bytesRead.Load(),
		BytesWritten:       e.metrics.bytesWritten.Load(),

		RecvResumeWhileCancelPending: e.metrics.recvArm.resumeWhileCancelPending.Load(),
		RecvResumeWhileRecvInFlight:  e.metrics.recvArm.resumeWhileRecvInFlight.Load(),
		RecvArmDeclined:              e.metrics.recvArm.armDeclined.Load(),
		RecvDoubleArmed:              e.metrics.recvArm.doubleArmed.Load(),
		RecvCQEUnaccounted:           e.metrics.recvArm.cqeUnaccounted.Load(),
		RecvSQFull:                   e.metrics.recvArm.sqFullRecv.Load(),
		RecvStallEpisodes:            e.metrics.recvArm.stallEpisodes.Load(),
		RecvStallNanos:               e.metrics.recvArm.stallNanos.Load(),
		RecvStallMaxNanos:            e.metrics.recvArm.stallMaxNanos.Load(),
		RecvLinkedArms:               e.metrics.recvArm.linkedRecvArms.Load(),
		RecvLinkedBlockedNanos:       e.metrics.recvArm.linkedRecvBlockedNanos.Load(),
		RecvLinkedBlockedMaxNanos:    e.metrics.recvArm.linkedRecvBlockedMax.Load(),
		DetachedConnections:          e.metrics.detachedConns.Load(),
		DetachWindowCloses:           e.metrics.detachWindowCloses.Load(),
		ZCSendsSubmitted:             e.metrics.zc.submits.Load(),
		ZCNotifs:                     e.metrics.zc.notifs.Load(),
		InlineBytes:                  e.metrics.zc.inlineBytes.Load(),
		RingBytes:                    e.metrics.zc.ringBytes.Load(),

		TransplantAdopted:           e.metrics.transplantCount.Load(),
		TransplantDetached:          e.metrics.transplantDetached.Load(),
		TransplantAdoptSlotOccupied: e.metrics.transplantSlotOccupied.Load(),
		TransplantHandoffRefused:    e.metrics.transplantHandoffRefused.Load(),
		TransplantAdoptRefused:      e.metrics.transplantAdoptRefused.Load(),
		CloseMissingConnState:       e.metrics.closeMissingConnState.Load(),

		StaleRecvDataClosed:       e.metrics.handoffLoss.staleRecvDataClosed.Load(),
		StaleRecvDataTransplanted: e.metrics.handoffLoss.staleRecvDataTransplanted.Load(),
		StaleRecvDataUnattributed: e.metrics.handoffLoss.staleRecvDataUnattributed.Load(),
		TransplantHandoffInFlight: e.metrics.handoffLoss.handoffInFlight.Load(),
		TransplantHeld:            e.metrics.handoffLoss.held.Load(),
		TransplantReaps:           e.metrics.handoffLoss.reaps.Load(),
		TransplantReapMisses:      e.metrics.handoffLoss.reapMisses.Load(),
		TransplantHoldRescued:     e.metrics.handoffLoss.holdRescued.Load(),
		TransplantDoubleClaim:     e.metrics.handoffLoss.doubleClaim.Load(),
		TransplantClaimDeferred:   e.metrics.handoffLoss.claimDeferred.Load(),
		TransplantReapFailed:      e.metrics.handoffLoss.reapFailed.Load(),
		TransplantReapUnsupported: e.metrics.handoffLoss.reapUnsupported.Load(),

		TransplantSweepPasses:       e.metrics.sweep.passes.Load(),
		TransplantResidualDetached:  e.metrics.sweep.residual[resDetached].Load(),
		TransplantResidualH2:        e.metrics.sweep.residual[resH2].Load(),
		TransplantResidualPinned:    e.metrics.sweep.residual[resPinned].Load(),
		TransplantResidualUnstarted: e.metrics.sweep.residual[resUnstarted].Load(),
		TransplantResidualBusy:      e.metrics.sweep.residual[resBusy].Load(),
	}
	// ErrorCount and its eleven buckets, together, from one snapshot
	// (celeris#645).
	engine.FillErrorClasses(&m, e.metrics.errs.Snapshot())
	return m
}

// Type returns the engine type.
func (e *Engine) Type() engine.EngineType {
	return engine.IOUring
}

// BeginPauseAccept starts pausing accept and returns without waiting
// (celeris#662). Each worker then, on its own thread, clears
// TCP_DEFER_ACCEPT on its listener, keeps its accept armed and keeps serving
// for deferlinger.Linger after that clear, and cancels the accept and closes
// the listener once it has drained the accept queue. A connection whose
// handshake completed before the pause but which has sent nothing yet is
// promoted by the kernel during that linger and served like any other,
// instead of being reset by the close.
//
// It does not promise a prompt start: a worker sees the flag at its next ring
// wakeup, which on an idle plain-HTTP/1 worker can be up to 100 ms away.
// Correctness does not depend on it, because each listener's deadline is
// taken at its own clear.
//
// It reads net.ipv4.tcp_synack_retries once, here, for the workers' guard
// (see deferlinger). The adaptive engine calls it for the sub-engine a
// switch leaves, so a switch does not wait for the linger.
func (e *Engine) BeginPauseAccept() {
	e.pause.Begin(e.cfg.Logger, "io_uring")
	e.acceptPaused.Store(true)
}

// PauseAccept stops accepting new connections. It is BeginPauseAccept
// followed by a wait, and it returns once every worker has cancelled its
// accept and closed its listen socket, so the SO_REUSEPORT group has shed
// this engine; or once a ResumeAccept has withdrawn the pause; or once every
// worker has exited; or, best effort, after deferlinger.Linger plus one
// second.
//
// It therefore takes about deferlinger.Linger (1.5 s), during which the
// engine keeps admitting connections: that is how a connection that
// completed its handshake before the pause but had not sent its request
// yet is served rather than reset (celeris#662, celeris#675). Connections
// already accepted continue to be served, and those still in a worker's
// accept queue at the close are accepted and served too. A listener built
// with resource.Config.DisableDeferAccept has nothing to linger for and
// closes at once.
func (e *Engine) PauseAccept() error {
	e.BeginPauseAccept()
	e.mu.Lock()
	workers := append([]*Worker(nil), e.workers...)
	e.mu.Unlock()
	if len(workers) == 0 {
		return nil
	}
	start := time.Now()
	deadline := start.Add(max(deferlinger.Linger(), 0) + time.Second)
	for {
		if !e.acceptPaused.Load() {
			return nil // a ResumeAccept withdrew the pause
		}
		allClosed := true
		for _, w := range workers {
			if !w.listenFDClosed.Load() {
				allClosed = false
				break
			}
		}
		if allClosed {
			return nil
		}
		if time.Now().After(deadline) {
			return nil // best-effort: do not surface the timeout, the FD will close shortly
		}
		time.Sleep(pausePollInterval(time.Since(start)))
	}
}

// pausePollInterval paces PauseAccept's wait: fine-grained for a pause that
// closes at once (DisableDeferAccept, or a test's zero linger), coarse over
// the linger so a 1.5 s wait does not spin.
func pausePollInterval(elapsed time.Duration) time.Duration {
	if elapsed < 20*time.Millisecond {
		return 100 * time.Microsecond
	}
	return time.Millisecond
}

// ResumeAccept starts accepting new connections again.
// Wakes any suspended workers so they re-create listen sockets.
func (e *Engine) ResumeAccept() error {
	e.acceptPaused.Store(false)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, w := range e.workers {
		// Re-arm the close-confirmation flag so a subsequent Pause cycle
		// blocks on the new listen FD rather than the previously-closed
		// one.
		w.listenFDClosed.Store(false)
		w.wakeIfSuspended()
	}
	return nil
}

// wakeIfSuspended ends a DRAINING→SUSPENDED park: if the worker is parked,
// close its wake channel, give it a fresh one and clear suspended. Safe from
// any goroutine; a no-op on a worker that is running.
//
// The park waits on a Go channel, not on the ring, so an eventfd write does
// not end it — only this does. ResumeAccept is one caller. The driver-action
// queue is the other: AdoptConn (and a driver's RegisterConn / Write /
// UnregisterConn) arrives from another goroutine while the worker has no
// connection and no listener to wake it for, and without the kick a standby
// worker slept through a transplanted descriptor until the next ResumeAccept —
// forever, if the engine stayed the standby — while the source already counted
// the hand-off done (celeris#658).
//
// No wakeup can be lost. A waker publishes its work — sets the flag the park
// watches — BEFORE it takes wakeMu, and the worker re-checks those flags UNDER
// wakeMu before it sets suspended. The mutex serialises the two: if the
// worker's check runs first, the waker finds suspended set and closes wake; if
// the waker's runs first, the flag is already set and the worker does not park.
//
// Lock order: e.mu → wakeMu (ResumeAccept) is the only nesting. wakeMu is a
// leaf — nothing is acquired while it is held — so no caller may hold
// driverActionMu or detachQMu when it calls this.
func (w *Worker) wakeIfSuspended() {
	w.wakeMu.Lock()
	if w.suspended.Load() {
		close(w.wake)
		w.wake = make(chan struct{})
		w.suspended.Store(false)
	}
	w.wakeMu.Unlock()
}

var (
	_ engine.Engine            = (*Engine)(nil)
	_ engine.AcceptController  = (*Engine)(nil)
	_ engine.EventLoopProvider = (*Engine)(nil)
)

// NumWorkers returns the number of worker event loops available for
// driver FD registration.
func (e *Engine) NumWorkers() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.workers)
}

// WorkerLoop returns the WorkerLoop for worker n. Out-of-range n is
// reduced modulo NumWorkers so callers can hash a connection / FD across
// the available pool without first reading NumWorkers — important now
// that workers can be capped below NumCPU by RLIMIT_MEMLOCK.
// Negative n is mirrored to the positive side first. Panics only when
// the engine has no workers (i.e. Listen has not yet started).
func (e *Engine) WorkerLoop(n int) engine.WorkerLoop {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := len(e.workers)
	if count == 0 {
		panic("celeris/iouring: WorkerLoop called before Listen")
	}
	idx := n % count
	if idx < 0 {
		idx += count
	}
	return e.workers[idx]
}

// Addr returns the bound listener address.
func (e *Engine) Addr() net.Addr {
	if p := e.addr.Load(); p != nil {
		return *p
	}
	return nil
}
