//go:build linux

// Package iouring implements an asynchronous network I/O engine backed by Linux io_uring.
//
// # Environment Knobs
//
// The engine recognizes several environment variables for operator control and CI matrix testing:
//
//   - CELERIS_IOURING_SEND_ZC: Controls io_uring zero-copy send (IORING_OP_SEND_ZC).
//     Values: "on" ("1", "true") forces zero-copy send on if the functional probe passed;
//     "off" ("0", "false") forces plain SEND; "auto" or unset preserves current default behavior
//     (enabled on kernels where the functional probe passes). Final default decision pending
//     measured cluster A/B benchmarks (celeris#465).
//
//   - CELERIS_IOURING_MULTISHOT_RECV: Opts into multishot receive with provided buffer rings
//     (IORING_REGISTER_PBUF_RING + IORING_RECV_MULTISHOT). Set to "1" to enable. Disabled by default.
//
//   - CELERIS_IOURING_PBUF_COUNT: Overrides the auto-scaled provided-buffer-ring size per worker.
//     Must be a power of 2 (e.g. 1024, 2048, 4096); values are clamped to [16, 32768].
//     Non-power-of-2 values cause ring registration failure and automatic fallback to
//     single-shot per-connection buffers.
//
//   - CELERIS_MAX_IOURING_TIER: Caps the detected io_uring tier at startup ("none", "base", "high",
//     "optional"). Used primarily by CI to exercise lower-tier fallback paths on modern kernels.
package iouring
