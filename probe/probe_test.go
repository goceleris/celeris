package probe

import (
	"errors"
	"runtime"
	"testing"

	"github.com/goceleris/celeris/engine"
)

func mockProber(kernelVersion string, ioUringErr error, epoll bool, capSysNice bool, numaNodes int) *SyscallProber {
	return &SyscallProber{
		ReadKernelVersion: func() (string, error) { return kernelVersion, nil },
		ProbeIOUring: func() (uint32, []uint8, error) {
			if ioUringErr != nil {
				return 0, nil, ioUringErr
			}
			return 0, nil, nil
		},
		ProbeEpoll:      func() bool { return epoll },
		CheckCapSysNice: func() bool { return capSysNice },
		ReadNUMANodes:   func() int { return numaNodes },
	}
}

func TestProbeWithOptionalTier(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := mockProber("6.1.0-18-generic", nil, true, true, 2)
	profile := ProbeWith(sp)

	if profile.IOUringTier != engine.Optional {
		t.Fatalf("expected Optional tier, got %s", profile.IOUringTier)
	}
	if !profile.MultishotAccept {
		t.Fatal("expected MultishotAccept")
	}
	if !profile.MultishotRecv {
		t.Fatal("expected MultishotRecv")
	}
	if !profile.ProvidedBuffers {
		t.Fatal("expected ProvidedBuffers")
	}
	if !profile.CoopTaskrun {
		t.Fatal("expected CoopTaskrun")
	}
	if !profile.SingleIssuer {
		t.Fatal("expected SingleIssuer")
	}
	if !profile.LinkedSQEs {
		t.Fatal("expected LinkedSQEs")
	}
	if !profile.SQPoll {
		t.Fatal("expected SQPoll with CAP_SYS_NICE")
	}
	if !profile.EpollAvailable {
		t.Fatal("expected EpollAvailable")
	}
	if profile.NUMANodes != 2 {
		t.Fatalf("expected 2 NUMA nodes, got %d", profile.NUMANodes)
	}
}

func TestProbeWithHighTier(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := mockProber("5.19.0", nil, true, false, 1)
	profile := ProbeWith(sp)

	if profile.IOUringTier != engine.High {
		t.Fatalf("expected High tier, got %s", profile.IOUringTier)
	}
	if !profile.MultishotAccept {
		t.Fatal("expected MultishotAccept")
	}
	if !profile.MultishotRecv {
		t.Fatal("expected MultishotRecv")
	}
	if !profile.ProvidedBuffers {
		t.Fatal("expected ProvidedBuffers")
	}
	if profile.CoopTaskrun {
		t.Fatal("expected no CoopTaskrun")
	}
	if profile.SingleIssuer {
		t.Fatal("expected no SingleIssuer")
	}
}

func TestProbeWithMidTier(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := mockProber("5.13.0", nil, true, false, 1)
	profile := ProbeWith(sp)

	if profile.IOUringTier != engine.Mid {
		t.Fatalf("expected Mid tier, got %s", profile.IOUringTier)
	}
	if !profile.ProvidedBuffers {
		t.Fatal("expected ProvidedBuffers")
	}
	if profile.MultishotAccept {
		t.Fatal("expected no MultishotAccept")
	}
}

func TestProbeWithBaseTier(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := mockProber("5.10.0", nil, true, false, 1)
	profile := ProbeWith(sp)

	if profile.IOUringTier != engine.Base {
		t.Fatalf("expected Base tier, got %s", profile.IOUringTier)
	}
	if !profile.LinkedSQEs {
		t.Fatal("expected LinkedSQEs")
	}
	if profile.ProvidedBuffers {
		t.Fatal("expected no ProvidedBuffers")
	}
}

func TestProbeWithNoneTier(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := mockProber("4.19.0", nil, true, false, 1)
	profile := ProbeWith(sp)

	if profile.IOUringTier != engine.None {
		t.Fatalf("expected None tier, got %s", profile.IOUringTier)
	}
	if !profile.EpollAvailable {
		t.Fatal("expected EpollAvailable on 4.x kernel")
	}
}

func TestProbeNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this test checks non-linux behavior")
	}
	profile := Probe()

	if profile.IOUringTier != engine.None {
		t.Fatalf("expected None tier on non-linux, got %s", profile.IOUringTier)
	}
	if profile.EpollAvailable {
		t.Fatal("expected no epoll on non-linux")
	}
	if profile.OS != runtime.GOOS {
		t.Fatalf("expected OS=%s, got %s", runtime.GOOS, profile.OS)
	}
	if profile.NumCPU < 1 {
		t.Fatal("expected at least 1 CPU")
	}
	if profile.NUMANodes < 1 {
		t.Fatal("expected at least 1 NUMA node")
	}
}

func TestProbeIOUringFailureGracefulDegradation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := &SyscallProber{
		ReadKernelVersion: func() (string, error) { return "5.19.0", nil },
		ProbeIOUring: func() (uint32, []uint8, error) {
			return 0, nil, errors.New("ENOSYS")
		},
		ProbeEpoll:    func() bool { return true },
		ReadNUMANodes: func() int { return 1 },
	}
	profile := ProbeWith(sp)

	if profile.IOUringTier != engine.None {
		t.Fatalf("expected None tier on io_uring failure, got %s", profile.IOUringTier)
	}
	if !profile.EpollAvailable {
		t.Fatal("expected EpollAvailable even when io_uring fails")
	}
}

func TestProbeNUMANodes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := mockProber("5.10.0", nil, true, false, 4)
	profile := ProbeWith(sp)

	if profile.NUMANodes != 4 {
		t.Fatalf("expected 4 NUMA nodes, got %d", profile.NUMANodes)
	}
}

func TestProbeNUMANodesZeroDefaultsToOne(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := mockProber("5.10.0", nil, true, false, 0)
	profile := ProbeWith(sp)

	if profile.NUMANodes != 1 {
		t.Fatalf("expected 1 NUMA node (default), got %d", profile.NUMANodes)
	}
}
