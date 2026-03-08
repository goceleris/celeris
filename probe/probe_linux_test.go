//go:build linux

package probe

import (
	"testing"

	"github.com/goceleris/celeris/engine"
)

func TestProbeIntegration(t *testing.T) {
	profile := Probe()

	if profile.OS != "linux" {
		t.Fatalf("expected OS=linux, got %s", profile.OS)
	}
	if profile.NumCPU < 1 {
		t.Fatal("expected at least 1 CPU")
	}
	if profile.NUMANodes < 1 {
		t.Fatal("expected at least 1 NUMA node")
	}
	if profile.KernelVersion == "" {
		t.Fatal("expected non-empty KernelVersion")
	}
	if profile.KernelMajor < 1 {
		t.Fatal("expected KernelMajor >= 1")
	}

	// Epoll should be available on any modern Linux
	if !profile.EpollAvailable {
		t.Fatal("expected EpollAvailable on Linux")
	}

	t.Logf("probe result: tier=%s kernel=%s cpus=%d numa=%d epoll=%t",
		profile.IOUringTier, profile.KernelVersion, profile.NumCPU, profile.NUMANodes, profile.EpollAvailable)

	if profile.IOUringTier.Available() {
		if profile.IOUringTier < engine.Base {
			t.Fatal("io_uring available but tier < Base")
		}
		t.Logf("io_uring features: multishot_accept=%t multishot_recv=%t provided_buffers=%t sqpoll=%t coop_taskrun=%t single_issuer=%t linked_sqes=%t",
			profile.MultishotAccept, profile.MultishotRecv, profile.ProvidedBuffers,
			profile.SQPoll, profile.CoopTaskrun, profile.SingleIssuer, profile.LinkedSQEs)
	}
}
