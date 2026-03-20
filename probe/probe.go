package probe

import (
	"os"
	"runtime"

	"github.com/goceleris/celeris/engine"
)

// Probe detects system capabilities using the platform-default syscall prober.
// If CELERIS_MAX_IOURING_TIER is set (none/base/mid/high/optional), the
// detected io_uring tier and associated features are capped at that level.
// This allows CI to exercise every tier's code path on modern kernels.
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
	case "mid":
		return engine.Mid
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
		p.SingleIssuer = false
	}
	if maxTier < engine.High {
		p.MultishotAccept = false
		p.MultishotRecv = false
		p.ProvidedBuffers = false
		p.FixedFiles = false
		p.DeferTaskrun = false
	}
	if maxTier < engine.Mid {
		p.CoopTaskrun = false
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

	if kv.AtLeast(2, 6) && sp.ProbeEpoll != nil {
		profile.EpollAvailable = sp.ProbeEpoll()
	}

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

			if tier >= engine.Optional && sp.CheckCapSysNice != nil {
				profile.SQPoll = sqpoll || sp.CheckCapSysNice()
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
