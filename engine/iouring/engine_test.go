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
	if tier.SupportsFixedFiles() {
		t.Error("base tier should not support fixed files")
	}
	if tier.SupportsMultishotRecv() {
		t.Error("base tier should not support multishot recv")
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
	if tier.SupportsFixedFiles() {
		t.Error("mid tier should not support fixed files")
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
	if tier.SupportsMultishotRecv() {
		t.Error("high tier should not support multishot recv (disabled for cache locality)")
	}
	// Without FixedFiles in profile, should not support.
	if tier.SupportsFixedFiles() {
		t.Error("high tier without FixedFiles profile should not support fixed files")
	}
}

func TestSelectTierHighWithFixedFiles(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier:     engine.High,
		CoopTaskrun:     true,
		ProvidedBuffers: true,
		MultishotAccept: true,
		MultishotRecv:   true,
		SingleIssuer:    true,
		FixedFiles:      true,
	}
	tier := SelectTier(profile)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if !tier.SupportsFixedFiles() {
		t.Error("high tier with FixedFiles should support fixed files")
	}
}

func TestSelectTierHighWithDeferTaskrun(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier:     engine.High,
		CoopTaskrun:     true,
		ProvidedBuffers: true,
		MultishotAccept: true,
		MultishotRecv:   true,
		SingleIssuer:    true,
		DeferTaskrun:    true,
	}
	tier := SelectTier(profile)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	flags := tier.SetupFlags()
	if flags&setupDeferTaskrun == 0 {
		t.Error("expected DEFER_TASKRUN in setup flags")
	}
	if flags&setupCoopTaskrun != 0 {
		t.Error("DEFER_TASKRUN should replace COOP_TASKRUN")
	}
	if flags&setupSingleIssuer == 0 {
		t.Error("expected SINGLE_ISSUER (required by DEFER_TASKRUN)")
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

func TestSelectTierOptionalWithDeferTaskrun(t *testing.T) {
	// SQPOLL + DEFER_TASKRUN is incompatible (kernel returns EINVAL).
	// Optional tier must always use COOP_TASKRUN regardless of DeferTaskrun profile.
	profile := engine.CapabilityProfile{
		IOUringTier:     engine.Optional,
		CoopTaskrun:     true,
		ProvidedBuffers: true,
		MultishotAccept: true,
		MultishotRecv:   true,
		SingleIssuer:    true,
		SQPoll:          true,
		DeferTaskrun:    true,
		FixedFiles:      true,
	}
	tier := SelectTier(profile)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	flags := tier.SetupFlags()
	if flags&setupDeferTaskrun != 0 {
		t.Error("DEFER_TASKRUN must not be set with SQPOLL (incompatible)")
	}
	if flags&setupCoopTaskrun == 0 {
		t.Error("expected COOP_TASKRUN with SQPOLL")
	}
	if flags&setupSQPoll == 0 {
		t.Error("expected SQPOLL in setup flags")
	}
	if !tier.SupportsFixedFiles() {
		t.Error("expected fixed files support")
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
