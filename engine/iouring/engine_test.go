//go:build linux

package iouring

import (
	"testing"

	"github.com/goceleris/celeris/engine"
)

func TestSelectTierBase(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier: engine.Base,
	}
	tier := SelectTier(profile)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if tier.Tier() != engine.Base {
		t.Errorf("expected Base tier, got %v", tier.Tier())
	}
	if tier.SetupFlags() != 0 {
		t.Errorf("expected 0 flags for base tier")
	}
}

func TestSelectTierMid(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier: engine.Mid,
		CoopTaskrun: true,
	}
	tier := SelectTier(profile)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if tier.Tier() != engine.Mid {
		t.Errorf("expected Mid tier, got %v", tier.Tier())
	}
}

func TestSelectTierHigh(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier:     engine.High,
		CoopTaskrun:     true,
		ProvidedBuffers: true,
		MultishotAccept: true,
		MultishotRecv:   true,
		SingleIssuer:    true,
	}
	tier := SelectTier(profile)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if tier.Tier() != engine.High {
		t.Errorf("expected High tier, got %v", tier.Tier())
	}
	if !tier.SupportsProvidedBuffers() {
		t.Error("expected provided buffers support")
	}
	if !tier.SupportsMultishotAccept() {
		t.Error("expected multishot accept support")
	}
}

func TestSelectTierOptional(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier:     engine.Optional,
		CoopTaskrun:     true,
		ProvidedBuffers: true,
		MultishotAccept: true,
		MultishotRecv:   true,
		SingleIssuer:    true,
		SQPoll:          true,
	}
	tier := SelectTier(profile)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if tier.Tier() != engine.Optional {
		t.Errorf("expected Optional tier, got %v", tier.Tier())
	}
}

func TestSelectTierNone(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier: engine.None,
	}
	tier := SelectTier(profile)
	if tier != nil {
		t.Errorf("expected nil tier for None")
	}
}

func TestUserDataEncoding(t *testing.T) {
	tests := []struct {
		op uint64
		fd int
	}{
		{udAccept, 42},
		{udRecv, 1000},
		{udSend, 65535},
		{udClose, 0},
	}
	for _, tt := range tests {
		ud := encodeUserData(tt.op, tt.fd)
		if decodeOp(ud) != tt.op {
			t.Errorf("op mismatch: got %x, want %x", decodeOp(ud), tt.op)
		}
		if decodeFD(ud) != tt.fd {
			t.Errorf("fd mismatch: got %d, want %d", decodeFD(ud), tt.fd)
		}
	}
}
