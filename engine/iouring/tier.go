//go:build linux

package iouring

import (
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
	SQPollIdle() uint32
}

// SelectTier returns the highest available tier strategy for the given profile.
func SelectTier(profile engine.CapabilityProfile) TierStrategy {
	switch {
	case profile.IOUringTier >= engine.Optional && profile.SQPoll:
		return &optionalTier{
			sqPollIdle:   2000,
			deferTaskrun: profile.DeferTaskrun,
			fixedFiles:   profile.FixedFiles,
		}
	case profile.IOUringTier >= engine.High && profile.ProvidedBuffers:
		return &highTier{
			deferTaskrun: profile.DeferTaskrun,
			fixedFiles:   profile.FixedFiles,
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
// With kernel 6.1+: adds DEFER_TASKRUN (replaces COOP_TASKRUN), fixed files,
// and multishot recv with ring-mapped provided buffers.
type highTier struct {
	deferTaskrun bool
	fixedFiles   bool
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

func (t *highTier) PrepareRecv(ring *Ring, fd int, buf []byte) {
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
	prepSend(sqe, fd, buf, linked)
	if t.fixedFiles {
		setSQEFixedFile(sqe)
	}
	setSQEUserData(sqe, encodeUserData(udSend, fd))
}

// optionalTier: kernel 6.0+, adds SQPOLL. With 6.1+: DEFER_TASKRUN, fixed files,
// multishot recv.
type optionalTier struct {
	sqPollIdle   uint32
	deferTaskrun bool
	fixedFiles   bool
}

func (t *optionalTier) Tier() engine.Tier { return engine.Optional }
func (t *optionalTier) SetupFlags() uint32 {
	if t.deferTaskrun {
		return setupDeferTaskrun | setupSingleIssuer | setupSQPoll
	}
	return setupCoopTaskrun | setupSingleIssuer | setupSQPoll
}
func (t *optionalTier) SupportsProvidedBuffers() bool { return true }
func (t *optionalTier) SupportsMultishotAccept() bool { return true }
func (t *optionalTier) SupportsMultishotRecv() bool   { return true }
func (t *optionalTier) SupportsFixedFiles() bool      { return t.fixedFiles }
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

func (t *optionalTier) PrepareRecv(ring *Ring, fd int, buf []byte) {
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
	prepSend(sqe, fd, buf, linked)
	if t.fixedFiles {
		setSQEFixedFile(sqe)
	}
	setSQEUserData(sqe, encodeUserData(udSend, fd))
}
