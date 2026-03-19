package probe

import (
	"runtime"

	"github.com/goceleris/celeris/engine"
)

// Probe detects system capabilities using the platform-default syscall prober.
func Probe() engine.CapabilityProfile {
	return ProbeWith(defaultProber())
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
