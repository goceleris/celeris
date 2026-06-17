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

// TestSelectTierMidCollapsesToBase pins the v1.4.8 retirement of the
// `Mid` tier (celeris#287). 5.13–5.18 kernels no longer carry their own
// strategy — they share `baseTier` because COOP_TASKRUN, the only
// feature `Mid` used to gate, doesn't actually exist before 5.19.
// Backwards-compat test: a profile reporting tier=Base with CoopTaskrun
// (which determineTier post-fix will never produce, but a hand-built
// profile might) selects baseTier and does not surface CoopTaskrun.
func TestSelectTierMidCollapsesToBase(t *testing.T) {
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
	if tier.SupportsFixedFiles() {
		t.Error("base tier should not support fixed files")
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
	if !tier.SupportsMultishotRecv() {
		t.Error("high tier should support multishot recv (6-8% throughput improvement)")
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
	tier := SelectTier(profile, 0)
	if tier == nil {
		t.Fatal("expected non-nil tier")
	}
	if !tier.SupportsFixedFiles() {
		t.Error("fixed files should be enabled when profile.FixedFiles is true")
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

func TestSelectTierOptionalUsesTaskrunNotSQPoll(t *testing.T) {
	// #377: SQPOLL is never selected. Per-worker rings would spawn one kernel
	// poll thread per worker (N spinning cores), and the dormant SQPOLL submit
	// path has a latent SQ-tail-publish race. The optional tier uses the
	// task-run completion model like the high tier instead.
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
	if flags&setupSQPoll != 0 {
		t.Error("SQPOLL must not be selected (#377)")
	}
	if flags&setupDeferTaskrun == 0 {
		t.Error("expected DEFER_TASKRUN when available")
	}
	if flags&setupSingleIssuer == 0 {
		t.Error("expected SINGLE_ISSUER")
	}
	if tier.SQPollIdle() != 0 {
		t.Error("SQPollIdle must be 0 when SQPOLL is disabled")
	}
	if !tier.SupportsFixedFiles() {
		t.Error("fixed files should be enabled when profile.FixedFiles is true")
	}

	// Without DEFER_TASKRUN (e.g. a 6.0 kernel), fall back to COOP_TASKRUN —
	// still never SQPOLL.
	coopProfile := engine.CapabilityProfile{
		IOUringTier: engine.Optional,
		CoopTaskrun: true,
		SQPoll:      true,
	}
	coopFlags := SelectTier(coopProfile, 0).SetupFlags()
	if coopFlags&setupSQPoll != 0 {
		t.Error("SQPOLL must not be selected without DeferTaskrun either (#377)")
	}
	if coopFlags&setupCoopTaskrun == 0 {
		t.Error("expected COOP_TASKRUN when DEFER_TASKRUN unavailable")
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
