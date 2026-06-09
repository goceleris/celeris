package probe

import (
	"errors"
	"runtime"
	"testing"

	"github.com/goceleris/celeris/engine"
)

// mockProber returns a SyscallProber that reports all IORING_FEAT_*
// bits present. The default mock is for tests that don't care about
// the vendor-kernel cross-check (Finding 4); for tests that want to
// exercise the suspect-vendor path, use mockProberWithFeatures.
func mockProber(kernelVersion string, ioUringErr error, epoll bool, capSysNice bool, numaNodes int) *SyscallProber {
	return mockProberWithFeatures(kernelVersion, 0xFFFFFFFF, ioUringErr, epoll, capSysNice, numaNodes)
}

// mockProberWithFeatures lets the caller pin the IORING_FEAT_* bitmap.
// Tests for the cross-check at probe/tier.go:determineTier use this to
// simulate a vendor / backport kernel with diverged feature surface.
func mockProberWithFeatures(kernelVersion string, features uint32, ioUringErr error, epoll bool, capSysNice bool, numaNodes int) *SyscallProber {
	return &SyscallProber{
		ReadKernelVersion: func() (string, error) { return kernelVersion, nil },
		ProbeIOUring: func() (uint32, []uint8, error) {
			if ioUringErr != nil {
				return 0, nil, ioUringErr
			}
			return features, nil, nil
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
	if !profile.DeferTaskrun {
		t.Fatal("expected DeferTaskrun on kernel 6.1+")
	}
	if !profile.FixedFiles {
		t.Fatal("expected FixedFiles on kernel 5.19+")
	}
	if !profile.SQPoll {
		t.Fatal("expected SQPoll with CAP_SYS_NICE")
	}
	if !profile.SendZC {
		t.Fatal("expected SendZC on kernel 6.0+")
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
	if !profile.CoopTaskrun {
		t.Fatal("expected CoopTaskrun")
	}
	if !profile.FixedFiles {
		t.Fatal("expected FixedFiles on kernel 5.19+")
	}
	if profile.DeferTaskrun {
		t.Fatal("expected no DeferTaskrun on kernel 5.19")
	}
	if !profile.SingleIssuer {
		// SINGLE_ISSUER landed in 5.19 alongside COOP_TASKRUN — see
		// celeris#287 Finding 1 / Finding 2 and io_uring_setup(2).
		t.Fatal("expected SingleIssuer on 5.19")
	}
	if profile.SQPoll {
		t.Fatal("expected no SQPoll")
	}
	if profile.SendZC {
		t.Fatal("expected no SendZC on kernel 5.19")
	}
}

// TestProbeWith5_13Through5_18 pins the v1.4.8 fix for celeris#287:
// kernels in the 5.13–5.18 range no longer surface CoopTaskrun (the
// IORING_SETUP_COOP_TASKRUN flag does not exist before 5.19). They now
// land on Base tier; pre-v1.4.7 they incorrectly landed on Mid with
// CoopTaskrun=true and caused a noisy fall-back when io_uring_setup
// rejected the flag.
func TestProbeWith5_13Through5_18(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	// 5.13 with IORING_FEAT_RSRC_TAGS (1<<10) set, confirming a real
	// 5.13+ kernel rather than a vendor backport.
	sp := mockProberWithFeatures("5.13.0", 1<<10, nil, true, false, 1)
	profile := ProbeWith(sp)

	if profile.IOUringTier != engine.Base {
		t.Fatalf("expected Base tier on 5.13 (Mid retired in v1.4.8), got %s", profile.IOUringTier)
	}
	if profile.CoopTaskrun {
		t.Fatal("CoopTaskrun must be false on 5.13 — flag landed in 5.19")
	}
	if profile.ProvidedBuffers {
		t.Fatal("expected no ProvidedBuffers")
	}
	if profile.MultishotAccept {
		t.Fatal("expected no MultishotAccept")
	}
	if profile.FixedFiles {
		t.Fatal("expected no FixedFiles on kernel 5.13")
	}
	if profile.DeferTaskrun {
		t.Fatal("expected no DeferTaskrun on kernel 5.13")
	}
	if profile.SingleIssuer {
		t.Fatal("expected no SingleIssuer on 5.13 — flag landed in 5.19")
	}
}

// TestProbeWith5_15Ubuntu22_04 pins the production smoking gun that
// motivated celeris#287: Ubuntu 22.04 LTS ships kernel 5.15. Pre-fix,
// every Ubuntu 22.04 host attempted io_uring_setup with COOP_TASKRUN
// (EINVAL) and fell back noisily.
func TestProbeWith5_15Ubuntu22_04(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	sp := mockProberWithFeatures("5.15.0-generic", 1<<10, nil, true, false, 1)
	profile := ProbeWith(sp)
	if profile.IOUringTier != engine.Base {
		t.Fatalf("Ubuntu 22.04 (5.15) must land on Base, got %s", profile.IOUringTier)
	}
	if profile.CoopTaskrun {
		t.Fatal("Ubuntu 22.04 must not advertise CoopTaskrun (kernel 5.15, flag at 5.19)")
	}
}

// TestProbeWithSuspectVendorKernel exercises the Finding 4 defence:
// a kernel that claims via uname to be ≥ 5.13 but doesn't report
// IORING_FEAT_RSRC_TAGS in its features bitmap is clamped to Base.
// This guards against forked / backported kernels whose feature surface
// diverges from the upstream version table.
func TestProbeWithSuspectVendorKernel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	// 6.0 kernel claim but features=0 (no RSRC_TAGS) — treat as vendor
	// kernel with diverged surface.
	sp := mockProberWithFeatures("6.0.0", 0, nil, true, false, 1)
	profile := ProbeWith(sp)
	if profile.IOUringTier != engine.Base {
		t.Fatalf("suspect vendor kernel (claims 6.0, lacks RSRC_TAGS bit) must clamp to Base, got %s", profile.IOUringTier)
	}
	if profile.CoopTaskrun || profile.SingleIssuer || profile.SQPoll {
		t.Fatal("suspect vendor kernel must not surface 5.19+/6.0+ flags")
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

// TestProbeSendfileAndZerocopy pins the new sendfile / zerocopy
// capability flags (celeris#317). Sendfile is unconditional on Linux
// (kernel 2.6.23+, every distro past 2.6.33); Zerocopy requires kernel
// 5.0+ for TCP sendmsg.
func TestProbeSendfileAndZerocopy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tier detection requires linux GOOS")
	}
	tests := []struct {
		name   string
		kernel string
		wantSF bool
		wantZC bool
	}{
		{"4.19 (pre-zerocopy)", "4.19.0", true, false},
		{"5.0 (zerocopy boundary)", "5.0.0", true, true},
		{"5.10 (celer's LTS floor)", "5.10.0", true, true},
		{"6.6 (current LTS)", "6.6.0", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := mockProber(tt.kernel, nil, true, false, 1)
			p := ProbeWith(sp)
			if p.Sendfile != tt.wantSF {
				t.Errorf("kernel %s: Sendfile = %v, want %v", tt.kernel, p.Sendfile, tt.wantSF)
			}
			if p.Zerocopy != tt.wantZC {
				t.Errorf("kernel %s: Zerocopy = %v, want %v", tt.kernel, p.Zerocopy, tt.wantZC)
			}
		})
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
