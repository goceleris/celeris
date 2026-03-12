package engine

import "testing"

func TestNewDefaultProfile(t *testing.T) {
	p := NewDefaultProfile()
	if p.IOUringTier != None {
		t.Errorf("IOUringTier = %v, want None", p.IOUringTier)
	}
	if p.NUMANodes != 1 {
		t.Errorf("NUMANodes = %d, want 1", p.NUMANodes)
	}
	if p.EpollAvailable {
		t.Error("EpollAvailable = true, want false")
	}
	if p.NumCPU != 0 {
		t.Errorf("NumCPU = %d, want 0", p.NumCPU)
	}
	if p.OS != "" {
		t.Errorf("OS = %q, want empty", p.OS)
	}
	if p.KernelVersion != "" {
		t.Errorf("KernelVersion = %q, want empty", p.KernelVersion)
	}
	if p.MultishotAccept || p.MultishotRecv || p.ProvidedBuffers || p.SQPoll || p.CoopTaskrun || p.SingleIssuer || p.LinkedSQEs || p.DeferTaskrun || p.FixedFiles {
		t.Error("boolean capabilities should default to false")
	}
}

func TestCapabilityProfileCustomValues(t *testing.T) {
	p := CapabilityProfile{
		OS:              "linux",
		KernelVersion:   "6.1.0",
		KernelMajor:     6,
		KernelMinor:     1,
		IOUringTier:     High,
		EpollAvailable:  true,
		MultishotAccept: true,
		MultishotRecv:   true,
		ProvidedBuffers: true,
		SQPoll:          true,
		CoopTaskrun:     true,
		SingleIssuer:    true,
		LinkedSQEs:      true,
		DeferTaskrun:    true,
		FixedFiles:      true,
		NumCPU:          16,
		NUMANodes:       2,
	}
	if p.OS != "linux" {
		t.Errorf("OS = %q, want %q", p.OS, "linux")
	}
	if p.KernelMajor != 6 || p.KernelMinor != 1 {
		t.Errorf("kernel version = %d.%d, want 6.1", p.KernelMajor, p.KernelMinor)
	}
	if p.IOUringTier != High {
		t.Errorf("IOUringTier = %v, want High", p.IOUringTier)
	}
	if !p.EpollAvailable {
		t.Error("EpollAvailable = false, want true")
	}
	if p.NumCPU != 16 {
		t.Errorf("NumCPU = %d, want 16", p.NumCPU)
	}
	if p.NUMANodes != 2 {
		t.Errorf("NUMANodes = %d, want 2", p.NUMANodes)
	}
	if !p.MultishotAccept || !p.MultishotRecv || !p.ProvidedBuffers || !p.SQPoll || !p.CoopTaskrun || !p.SingleIssuer || !p.LinkedSQEs || !p.DeferTaskrun || !p.FixedFiles {
		t.Error("all boolean capabilities should be true")
	}
}
