package probe

import (
	"os"
	"runtime"

	"github.com/goceleris/celeris/engine"
)

// Probe detects system capabilities using the platform-default syscall prober.
// If CELERIS_MAX_IOURING_TIER is set (none/base/high/optional), the detected
// io_uring tier and associated features are capped at that level. This allows
// CI to exercise every tier's code path on modern kernels.
func Probe() engine.CapabilityProfile {
	profile := ProbeWith(defaultProber())
	if maxTier := os.Getenv("CELERIS_MAX_IOURING_TIER"); maxTier != "" {
		profile = capIOUringTier(profile, parseTierName(maxTier))
	}
	return profile
}

func parseTierName(s string) engine.Tier {
	switch s {
	case "optional":
		return engine.Optional
	case "high":
		return engine.High
	case "base":
		return engine.Base
	default:
		return engine.None
	}
}

// capIOUringTier clamps the profile's io_uring capabilities to at most maxTier.
func capIOUringTier(p engine.CapabilityProfile, maxTier engine.Tier) engine.CapabilityProfile {
	if p.IOUringTier <= maxTier {
		return p
	}
	p.IOUringTier = maxTier
	if maxTier < engine.Optional {
		p.SQPoll = false
		p.SendZC = false
		// SingleIssuer is cleared in the `maxTier < High` branch below
		// — it landed in 5.19 (High) alongside COOP_TASKRUN, not in 6.0
		// (Optional). See celeris#287.
	}
	if maxTier < engine.High {
		p.MultishotAccept = false
		p.MultishotRecv = false
		p.ProvidedBuffers = false
		p.FixedFiles = false
		p.DeferTaskrun = false
		// COOP_TASKRUN + SINGLE_ISSUER were introduced together in 5.19
		// and land at the High tier.
		p.CoopTaskrun = false
		p.SingleIssuer = false
	}
	if maxTier < engine.Base {
		p.LinkedSQEs = false
	}
	return p
}

// ProbeWith detects system capabilities using the provided SyscallProber.
func ProbeWith(sp *SyscallProber) engine.CapabilityProfile { //nolint:revive // ProbeWith is clearer than With for public API
	profile := engine.NewDefaultProfile()
	profile.OS = runtime.GOOS
	profile.NumCPU = runtime.NumCPU()

	if runtime.GOOS != "linux" {
		return profile
	}

	versionStr, err := sp.ReadKernelVersion()
	if err != nil {
		return profile
	}

	kv, err := ParseKernelVersion(versionStr)
	if err != nil {
		return profile
	}

	profile.KernelVersion = kv.String()
	profile.KernelMajor = kv.Major
	profile.KernelMinor = kv.Minor

	// sendfile(2) is universally available on Linux (kernel 2.6.33+ via
	// pipe + splice; kernel 2.6.23+ for the actual syscall, every distro
	// ships well past that). Set it true here unconditionally — the
	// runtime probe (probeSendfile) is a no-op because the syscall can't
	// fail at registration time, only at call time per file.
	profile.Sendfile = true
	// MSG_ZEROCOPY landed for TCP send paths in Linux 4.14 (commit
	// f214f915e7db); UDP support came later, in 5.0 (commit b5947e5d1e71).
	// celeris is TCP-only, so 4.14 would be the correct floor — BUT the
	// engine does not currently ship a MSG_ZEROCOPY send path (a correct
	// implementation needs SO_ZEROCOPY + sendmsg(MSG_ZEROCOPY) + errqueue
	// completion draining integrated into the event loop + buffer pinning;
	// see engine/epoll/sendfile.go). Leaving Zerocopy false avoids
	// advertising a capability the tree does not deliver. sendfile(2)
	// already provides zero-copy for the file-serving workload. Flip this
	// to `kv.AtLeast(4, 14)` only when the MSG_ZEROCOPY send path lands.
	profile.Zerocopy = false

	if kv.AtLeast(2, 6) && sp.ProbeEpoll != nil {
		profile.EpollAvailable = sp.ProbeEpoll()
	}

	// 5.10 is celeris's LTS-stable io_uring floor. The io_uring syscall
	// itself landed in 5.1, but pre-5.10 kernels carry pre-LTS-stability
	// io_uring bugs (deadlocks, credential races, registered-fd surprises)
	// that are not worth supporting in a production HTTP engine. 5.10 is
	// the cut-off: Debian 11, RHEL 8.5+, and every distro since carry
	// 5.10+. Kernels below 5.10 fall through to the epoll path above.
	// See celeris#287 Finding 3.
	if kv.AtLeast(5, 10) && sp.ProbeIOUring != nil {
		features, ops, err := sp.ProbeIOUring()
		if err == nil {
			tier, multishotAccept, multishotRecv, providedBuffers, sqpoll, coopTaskrun, singleIssuer, linkedSQEs, deferTaskrun, fixedFiles, sendZC := determineTier(kv, features, ops)
			profile.IOUringTier = tier
			profile.MultishotAccept = multishotAccept
			profile.MultishotRecv = multishotRecv
			profile.ProvidedBuffers = providedBuffers
			profile.CoopTaskrun = coopTaskrun
			profile.SingleIssuer = singleIssuer
			profile.LinkedSQEs = linkedSQEs
			profile.DeferTaskrun = deferTaskrun
			profile.FixedFiles = fixedFiles
			profile.SendZC = sendZC

			// SQPoll (IORING_SETUP_SQPOLL) lands at the Optional tier
			// (kernel 6.0+), where `sqpoll` from determineTier is already
			// true. On 6.0+ the basic SQ-polling mode does not require
			// CAP_SYS_NICE (that requirement was for pinning the poll
			// thread to a CPU and for pre-5.13 kernels), so the tier flag
			// alone is the correct gate. The previous `sqpoll ||
			// CheckCapSysNice()` was a constant-true short-circuit
			// (CheckCapSysNice never ran) AND the old CheckCapSysNice
			// implementation mutated the process scheduling priority via
			// Setpriority — a probe must never have side effects. The
			// capability is now a genuine read-only check (see
			// probe_linux.go) and is consulted only when the tier itself
			// does not already enable SQPoll, so it can never re-disable a
			// supported feature.
			if tier >= engine.Optional {
				profile.SQPoll = sqpoll
				if !profile.SQPoll && sp.CheckCapSysNice != nil {
					profile.SQPoll = sp.CheckCapSysNice()
				}
			}
		}
	}

	if sp.ReadNUMANodes != nil {
		nodes := sp.ReadNUMANodes()
		if nodes > 0 {
			profile.NUMANodes = nodes
		}
	}

	return profile
}
