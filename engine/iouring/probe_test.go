//go:build linux

package iouring

import (
	"errors"
	"strings"
	"testing"
)

// TestNotifUsageZCCopiedConstant pins the kernel UAPI constant for
// IORING_NOTIF_USAGE_ZC_COPIED = (1U << 31) (celeris#465).
func TestNotifUsageZCCopiedConstant(t *testing.T) {
	const want uint32 = 1 << 31
	if notifUsageZCCopied != want {
		t.Fatalf("notifUsageZCCopied = %#x, want %#x (1<<31)", notifUsageZCCopied, want)
	}
}

// TestParseSendZCResult exercises all four probe outcomes against synthetic CQEs (celeris#465).
func TestParseSendZCResult(t *testing.T) {
	cases := []struct {
		name          string
		initialRes    int32
		initialFlags  uint32
		notifArrived  bool
		waitErr       error
		notifRes      int32
		notifFlags    uint32
		wantResult    SendZCProbeResult
		wantReasonSub string
	}{
		{
			name:          "unsupported-enosys",
			initialRes:    -38, // -ENOSYS
			initialFlags:  0,
			wantResult:    SendZCUnsupported,
			wantReasonSub: "kernel rejected SEND_ZC opcode",
		},
		{
			name:          "unsupported-einval",
			initialRes:    -22, // -EINVAL
			initialFlags:  0,
			wantResult:    SendZCUnsupported,
			wantReasonSub: "kernel rejected SEND_ZC opcode",
		},
		{
			name:          "copy-fallback-missing-f-more",
			initialRes:    64,
			initialFlags:  0, // missing CQE_F_MORE (0x02)
			wantResult:    SendZCCopyFallback,
			wantReasonSub: "missing CQE_F_MORE flag",
		},
		{
			name:          "broken-wait-timeout",
			initialRes:    64,
			initialFlags:  0x02, // CQE_F_MORE
			notifArrived:  false,
			waitErr:       errors.New("deadline exceeded"),
			wantResult:    SendZCBroken,
			wantReasonSub: "notification CQE wait timed out: deadline exceeded",
		},
		{
			name:          "broken-missing-notification",
			initialRes:    64,
			initialFlags:  0x02, // CQE_F_MORE
			notifArrived:  false,
			wantResult:    SendZCBroken,
			wantReasonSub: "no notification CQE produced",
		},
		{
			name:          "broken-missing-notif-flag",
			initialRes:    64,
			initialFlags:  0x02,
			notifArrived:  true,
			notifRes:      0,
			notifFlags:    0, // missing CQE_F_NOTIF (1<<3)
			wantResult:    SendZCBroken,
			wantReasonSub: "second CQE missing CQE_F_NOTIF flag",
		},
		{
			name:          "copy-fallback-zc-copied-bit31",
			initialRes:    64,
			initialFlags:  0x02,
			notifArrived:  true,
			notifRes:      int32(-2147483648), // 0x80000000: bit 31 set (IORING_NOTIF_USAGE_ZC_COPIED)
			notifFlags:    cqeFNotif,
			wantResult:    SendZCCopyFallback,
			wantReasonSub: "IORING_NOTIF_USAGE_ZC_COPIED",
		},
		{
			name:          "true-zero-copy-bit31-clear",
			initialRes:    64,
			initialFlags:  0x02,
			notifArrived:  true,
			notifRes:      0, // bit 31 clear
			notifFlags:    cqeFNotif,
			wantResult:    SendZCTrueZeroCopy,
			wantReasonSub: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRes, gotReason := parseSendZCResult(tc.initialRes, tc.initialFlags, tc.notifArrived, tc.waitErr, tc.notifRes, tc.notifFlags)
			if gotRes != tc.wantResult {
				t.Fatalf("parseSendZCResult() result = %v (%s), want %v (%s)",
					gotRes, gotRes.String(), tc.wantResult, tc.wantResult.String())
			}
			if tc.wantReasonSub != "" && !strings.Contains(gotReason, tc.wantReasonSub) {
				t.Fatalf("parseSendZCResult() reason = %q, want substring %q", gotReason, tc.wantReasonSub)
			}
		})
	}
}

// TestResolveSendZCPolicy verifies SEND_ZC policy gating under all options (celeris#465).
func TestResolveSendZCPolicy(t *testing.T) {
	cases := []struct {
		name       string
		functional bool
		envVal     string
		want       bool
	}{
		{"non-functional-env-on", false, "on", false},
		{"non-functional-env-1", false, "1", false},
		{"non-functional-env-auto", false, "auto", false},
		{"non-functional-env-empty", false, "", false},
		{"functional-env-on", true, "on", true},
		{"functional-env-1", true, "1", true},
		{"functional-env-true", true, "true", true},
		{"functional-env-off", true, "off", false},
		{"functional-env-0", true, "0", false},
		{"functional-env-false", true, "false", false},
		{"functional-env-auto", true, "auto", true},
		{"functional-env-empty", true, "", true},
		{"functional-env-unknown", true, "invalid", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSendZCPolicy(tc.functional, tc.envVal)
			if got != tc.want {
				t.Fatalf("resolveSendZCPolicy(%v, %q) = %v, want %v", tc.functional, tc.envVal, got, tc.want)
			}
		})
	}
}

// TestProbeSendZCLiveLoopback runs the real probe against loopback on Linux,
// confirming it accurately detects copy-fallback (celeris#465).
func TestProbeSendZCLiveLoopback(t *testing.T) {
	res, reason := probeSendZC()
	t.Logf("probeSendZC() live result: %v (%s), reason: %q", res, res.String(), reason)
	if res != SendZCCopyFallback {
		t.Fatalf("expected loopback probe to report SendZCCopyFallback, got %v (%s)", res, res.String())
	}
	if !strings.Contains(reason, "IORING_NOTIF_USAGE_ZC_COPIED") {
		t.Fatalf("expected reason mentioning IORING_NOTIF_USAGE_ZC_COPIED, got %q", reason)
	}
}
