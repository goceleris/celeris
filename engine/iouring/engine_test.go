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
	tier := SelectTier(profile, 0)
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
	tier := SelectTier(profile, 0)
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
	tier := SelectTier(profile, 0)
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
	// Fixed files are disabled (IU-1: TCP_NODELAY not set with ACCEPT_DIRECT).
	profile := engine.CapabilityProfile{
		IOUringTier:     engine.High,
		CoopTaskrun:     true,
		ProvidedBuffers: true,
		MultishotAccept: true,
		MultishotRecv:   true,
		SingleIssuer:    true,
		FixedFiles:      true,
	}
	tier := SelectTier(profile, 0)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if tier.SupportsFixedFiles() {
		t.Error("fixed files should be disabled (TCP_NODELAY bug)")
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
	tier := SelectTier(profile, 0)
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

func TestSelectTierDeferTaskrunPriorityOverSQPoll(t *testing.T) {
	// DEFER_TASKRUN (high tier) is prioritized over SQPOLL (optional tier)
	// because SQPOLL's kernel thread steals CPU from workers.
	profile := engine.CapabilityProfile{
		IOUringTier:     engine.Optional,
		CoopTaskrun:     true,
		ProvidedBuffers: true,
		MultishotAccept: true,
		MultishotRecv:   true,
		SingleIssuer:    true,
		SQPoll:          true,
	}
	tier := SelectTier(profile, 0)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if tier.Tier() != engine.High {
		t.Errorf("expected High tier (DEFER_TASKRUN priority), got %v", tier.Tier())
	}
}

func TestSelectTierOptional(t *testing.T) {
	// Optional tier selected when SQPoll available but ProvidedBuffers is not.
	profile := engine.CapabilityProfile{
		IOUringTier: engine.Optional,
		CoopTaskrun: true,
		SQPoll:      true,
	}
	tier := SelectTier(profile, 0)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if tier.Tier() != engine.Optional {
		t.Errorf("expected Optional tier, got %v", tier.Tier())
	}
}

func TestSelectTierOptionalWithSendZC(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier: engine.Optional,
		CoopTaskrun: true,
		SQPoll:      true,
		SendZC:      true,
	}
	tier := SelectTier(profile, 0)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if !tier.SupportsSendZC() {
		t.Error("expected send ZC support with SendZC profile")
	}
}

func TestSelectTierOptionalWithoutSendZC(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier: engine.Optional,
		CoopTaskrun: true,
		SQPoll:      true,
	}
	tier := SelectTier(profile, 0)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if tier.SupportsSendZC() {
		t.Error("should not support send ZC without SendZC profile")
	}
}

func TestSelectTierOptionalWithDeferTaskrun(t *testing.T) {
	// SQPOLL is incompatible with both DEFER_TASKRUN and COOP_TASKRUN.
	// SQPOLL path must use neither IPI-related flag.
	profile := engine.CapabilityProfile{
		IOUringTier:  engine.Optional,
		CoopTaskrun:  true,
		SQPoll:       true,
		DeferTaskrun: true,
		FixedFiles:   true,
	}
	tier := SelectTier(profile, 0)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	flags := tier.SetupFlags()
	if flags&setupDeferTaskrun != 0 {
		t.Error("DEFER_TASKRUN must not be set with SQPOLL (incompatible)")
	}
	if flags&setupCoopTaskrun != 0 {
		t.Error("COOP_TASKRUN must not be set with SQPOLL (incompatible)")
	}
	if flags&setupSQPoll == 0 {
		t.Error("expected SQPOLL in setup flags")
	}
	if tier.SupportsFixedFiles() {
		t.Error("fixed files should be disabled (TCP_NODELAY bug)")
	}
}

func TestSelectTierNone(t *testing.T) {
	profile := engine.CapabilityProfile{
		IOUringTier: engine.None,
	}
	tier := SelectTier(profile, 0)
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
