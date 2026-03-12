package probe

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/goceleris/celeris/engine"
)

// DiagnosticReport logs the capability profile as structured key-value pairs.
func DiagnosticReport(profile engine.CapabilityProfile, logger *slog.Logger) {
	logger.Info("capability probe complete",
		"os", profile.OS,
		"kernel", profile.KernelVersion,
		"io_uring_tier", profile.IOUringTier.String(),
		"epoll", profile.EpollAvailable,
		"cpus", profile.NumCPU,
		"numa_nodes", profile.NUMANodes,
	)

	if profile.IOUringTier.Available() {
		logger.Info("io_uring features",
			"multishot_accept", profile.MultishotAccept,
			"multishot_recv", profile.MultishotRecv,
			"provided_buffers", profile.ProvidedBuffers,
			"sqpoll", profile.SQPoll,
			"coop_taskrun", profile.CoopTaskrun,
			"single_issuer", profile.SingleIssuer,
			"linked_sqes", profile.LinkedSQEs,
			"defer_taskrun", profile.DeferTaskrun,
			"fixed_files", profile.FixedFiles,
		)
	}
}

// FormatDiagnostic returns a human-readable string representation of the capability profile.
func FormatDiagnostic(profile engine.CapabilityProfile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OS: %s\n", profile.OS)
	fmt.Fprintf(&b, "Kernel: %s (major=%d, minor=%d)\n", profile.KernelVersion, profile.KernelMajor, profile.KernelMinor)
	fmt.Fprintf(&b, "io_uring Tier: %s\n", profile.IOUringTier)
	fmt.Fprintf(&b, "epoll Available: %t\n", profile.EpollAvailable)
	fmt.Fprintf(&b, "CPUs: %d\n", profile.NumCPU)
	fmt.Fprintf(&b, "NUMA Nodes: %d\n", profile.NUMANodes)

	if profile.IOUringTier.Available() {
		fmt.Fprintf(&b, "MultishotAccept: %t\n", profile.MultishotAccept)
		fmt.Fprintf(&b, "MultishotRecv: %t\n", profile.MultishotRecv)
		fmt.Fprintf(&b, "ProvidedBuffers: %t\n", profile.ProvidedBuffers)
		fmt.Fprintf(&b, "SQPoll: %t\n", profile.SQPoll)
		fmt.Fprintf(&b, "CoopTaskrun: %t\n", profile.CoopTaskrun)
		fmt.Fprintf(&b, "SingleIssuer: %t\n", profile.SingleIssuer)
		fmt.Fprintf(&b, "LinkedSQEs: %t\n", profile.LinkedSQEs)
		fmt.Fprintf(&b, "DeferTaskrun: %t\n", profile.DeferTaskrun)
		fmt.Fprintf(&b, "FixedFiles: %t\n", profile.FixedFiles)
	}

	return b.String()
}
