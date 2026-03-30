//go:build linux

package iouring

import (
	"time"

	"github.com/goceleris/celeris/engine"
)

// TierStrategy configures io_uring behavior based on detected capabilities.
type TierStrategy interface {
	Tier() engine.Tier
	SetupFlags() uint32
	PrepareAccept(ring *Ring, listenFD int)
	PrepareRecv(ring *Ring, fd int, buf []byte)
	PrepareSend(ring *Ring, fd int, buf []byte, linked bool)
	SupportsProvidedBuffers() bool
	SupportsMultishotAccept() bool
	SupportsMultishotRecv() bool
	SupportsFixedFiles() bool
	SupportsSendZC() bool
	SQPollIdle() uint32
}

// SelectTier returns the highest available tier strategy for the given profile.
// sqPollIdle is the objective-specific SQPOLL thread idle timeout; if zero,
// defaults to 2000ms.
func SelectTier(profile engine.CapabilityProfile, sqPollIdle time.Duration) TierStrategy {
	switch {
	// DEFER_TASKRUN: completions run in worker's context (no extra kernel thread).
	// Preferred over SQPOLL because the SQPOLL kernel thread steals CPU from workers.
	// Benchmarks show DEFER_TASKRUN is 4% faster than SQPOLL on CPU-pinned workloads.
	case profile.IOUringTier >= engine.High && profile.ProvidedBuffers:
		return &highTier{
			deferTaskrun: profile.DeferTaskrun,
			fixedFiles:   profile.FixedFiles,
			sendZC:       profile.SendZC,
		}
	case profile.IOUringTier >= engine.Optional && profile.SQPoll:
		idle := uint32(sqPollIdle.Milliseconds())
		if idle == 0 {
			idle = 2000
		}
		return &optionalTier{
			sqPollIdle:   idle,
			deferTaskrun: profile.DeferTaskrun,
			fixedFiles:   profile.FixedFiles,
			sendZC:       profile.SendZC,
		}
	case profile.IOUringTier >= engine.Mid && profile.CoopTaskrun:
		return &midTier{}
	case profile.IOUringTier >= engine.Base:
		return &baseTier{}
	default:
		return nil
	}
}

// baseTier: kernel 5.10+, single-shot accept/recv, basic flags.
type baseTier struct{}

func (t *baseTier) Tier() engine.Tier             { return engine.Base }
func (t *baseTier) SetupFlags() uint32            { return 0 }
func (t *baseTier) SupportsProvidedBuffers() bool { return false }
func (t *baseTier) SupportsMultishotAccept() bool { return false }
func (t *baseTier) SupportsMultishotRecv() bool   { return false }
func (t *baseTier) SupportsFixedFiles() bool      { return false }
func (t *baseTier) SupportsSendZC() bool          { return false }
func (t *baseTier) SQPollIdle() uint32            { return 0 }

func (t *baseTier) PrepareAccept(ring *Ring, listenFD int) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	prepAccept(sqe, listenFD, 0)
	setSQEUserData(sqe, encodeUserData(udAccept, listenFD))
}

func (t *baseTier) PrepareRecv(ring *Ring, fd int, buf []byte) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	prepRecv(sqe, fd, buf)
	setSQEUserData(sqe, encodeUserData(udRecv, fd))
}

func (t *baseTier) PrepareSend(ring *Ring, fd int, buf []byte, linked bool) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	prepSend(sqe, fd, buf, linked)
	setSQEUserData(sqe, encodeUserData(udSend, fd))
}

// midTier: kernel 5.13+, adds COOP_TASKRUN.
type midTier struct{}

func (t *midTier) Tier() engine.Tier             { return engine.Mid }
func (t *midTier) SetupFlags() uint32            { return setupCoopTaskrun }
func (t *midTier) SupportsProvidedBuffers() bool { return false }
func (t *midTier) SupportsMultishotAccept() bool { return false }
func (t *midTier) SupportsMultishotRecv() bool   { return false }
func (t *midTier) SupportsFixedFiles() bool      { return false }
func (t *midTier) SupportsSendZC() bool          { return false }
func (t *midTier) SQPollIdle() uint32            { return 0 }

func (t *midTier) PrepareAccept(ring *Ring, listenFD int) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	prepAccept(sqe, listenFD, 0)
	setSQEUserData(sqe, encodeUserData(udAccept, listenFD))
}

func (t *midTier) PrepareRecv(ring *Ring, fd int, buf []byte) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	prepRecv(sqe, fd, buf)
	setSQEUserData(sqe, encodeUserData(udRecv, fd))
}

func (t *midTier) PrepareSend(ring *Ring, fd int, buf []byte, linked bool) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	prepSend(sqe, fd, buf, linked)
	setSQEUserData(sqe, encodeUserData(udSend, fd))
}

// highTier: kernel 5.19+, adds SINGLE_ISSUER, multishot accept, provided buffers.
// With kernel 6.1+: adds DEFER_TASKRUN (replaces COOP_TASKRUN), fixed files.
//
// Multishot recv with ring-mapped provided buffers is enabled: the kernel
// returns data in pre-registered pages, eliminating per-recv syscall overhead.
// Benchmarks show 6-8% throughput improvement over single-shot recv.
type highTier struct {
	deferTaskrun bool
	fixedFiles   bool
	sendZC       bool
}

func (t *highTier) Tier() engine.Tier { return engine.High }
func (t *highTier) SetupFlags() uint32 {
	if t.deferTaskrun {
		return setupDeferTaskrun | setupSingleIssuer
	}
	return setupCoopTaskrun | setupSingleIssuer
}
func (t *highTier) SupportsProvidedBuffers() bool { return true }
func (t *highTier) SupportsMultishotAccept() bool { return true }
func (t *highTier) SupportsMultishotRecv() bool   { return true }
func (t *highTier) SupportsFixedFiles() bool      { return t.fixedFiles }
func (t *highTier) SupportsSendZC() bool          { return t.sendZC }
func (t *highTier) SQPollIdle() uint32            { return 0 }

func (t *highTier) PrepareAccept(ring *Ring, listenFD int) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	if t.fixedFiles {
		prepMultishotAcceptDirect(sqe, listenFD)
	} else {
		prepMultishotAccept(sqe, listenFD)
	}
	setSQEUserData(sqe, encodeUserData(udAccept, listenFD))
}

func (t *highTier) PrepareRecv(ring *Ring, fd int, _ []byte) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	prepMultishotRecv(sqe, fd, 0, t.fixedFiles)
	setSQEUserData(sqe, encodeUserData(udRecv, fd))
}

func (t *highTier) PrepareSend(ring *Ring, fd int, buf []byte, linked bool) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	if t.sendZC && !linked {
		if t.fixedFiles {
			prepSendZCFixed(sqe, fd, buf, false)
		} else {
			prepSendZC(sqe, fd, buf, false)
		}
	} else {
		prepSend(sqe, fd, buf, linked)
		if t.fixedFiles {
			setSQEFixedFile(sqe)
		}
	}
	setSQEUserData(sqe, encodeUserData(udSend, fd))
}

// optionalTier: kernel 6.0+, adds SQPOLL, SEND_ZC. With 6.1+: DEFER_TASKRUN, fixed files.
type optionalTier struct {
	sqPollIdle   uint32
	deferTaskrun bool
	fixedFiles   bool
	sendZC       bool
}

func (t *optionalTier) Tier() engine.Tier { return engine.Optional }
func (t *optionalTier) SetupFlags() uint32 {
	// SQPOLL is incompatible with both DEFER_TASKRUN and COOP_TASKRUN.
	// SINGLE_ISSUER is compatible and enables SQ head optimization.
	return setupSQPoll | setupSingleIssuer
}
func (t *optionalTier) SupportsProvidedBuffers() bool { return true }
func (t *optionalTier) SupportsMultishotAccept() bool { return true }
func (t *optionalTier) SupportsMultishotRecv() bool   { return true }
func (t *optionalTier) SupportsFixedFiles() bool      { return t.fixedFiles }
func (t *optionalTier) SupportsSendZC() bool          { return t.sendZC }
func (t *optionalTier) SQPollIdle() uint32            { return t.sqPollIdle }

func (t *optionalTier) PrepareAccept(ring *Ring, listenFD int) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	if t.fixedFiles {
		prepMultishotAcceptDirect(sqe, listenFD)
	} else {
		prepMultishotAccept(sqe, listenFD)
	}
	setSQEUserData(sqe, encodeUserData(udAccept, listenFD))
}

func (t *optionalTier) PrepareRecv(ring *Ring, fd int, _ []byte) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	prepMultishotRecv(sqe, fd, 0, t.fixedFiles)
	setSQEUserData(sqe, encodeUserData(udRecv, fd))
}

func (t *optionalTier) PrepareSend(ring *Ring, fd int, buf []byte, linked bool) {
	sqe := ring.GetSQE()
	if sqe == nil {
		return
	}
	if t.sendZC && !linked {
		// SEND_ZC cannot be linked (the notification CQE would break
		// the link chain), so fall back to regular SEND for linked ops.
		if t.fixedFiles {
			prepSendZCFixed(sqe, fd, buf, false)
		} else {
			prepSendZC(sqe, fd, buf, false)
		}
	} else {
		prepSend(sqe, fd, buf, linked)
		if t.fixedFiles {
			setSQEFixedFile(sqe)
		}
	}
	setSQEUserData(sqe, encodeUserData(udSend, fd))
}
