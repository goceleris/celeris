//go:build linux

// Package iouring implements an asynchronous network I/O engine backed by Linux io_uring.
//
// # Environment Knobs
//
// The engine recognizes several environment variables, for operator control and for tests:
//
//   - CELERIS_IOURING_SEND_ZC: Controls io_uring zero-copy send (IORING_OP_SEND_ZC).
//     Values: "on" ("1", "true") forces zero-copy send on if the functional probe passed;
//     "off" ("0", "false") forces plain SEND; "auto" or unset enables it wherever the startup
//     functional probe passes, and "on" cannot enable it where the probe failed. Any other
//     value is treated as "auto". It is logged as a warning only when the probed profile has
//     SEND_ZC (the optional tier) and the functional probe passed, because only then is the
//     value examined. Whether "auto" should keep enabling it is an open measurement, owned by
//     celeris#585 (SEND_ZC on/off A/B on the real fabric).
//
//   - CELERIS_IOURING_MULTISHOT_RECV: Opts into multishot receive with provided buffer rings
//     (IORING_REGISTER_PBUF_RING + IORING_RECV_MULTISHOT). Set to "1" to enable. Disabled by default.
//
//   - CELERIS_IOURING_PBUF_COUNT: Overrides the auto-scaled provided-buffer-ring size per worker
//     (used only with multishot receive). A positive value is rounded up to the next power of
//     2 and clamped to [1024, 32768] (bufRingCountMin, bufRingCountMax); 0, a negative value or
//     a non-integer keeps the auto-scaled size. See resolveBufRingCount.
//
//   - CELERIS_MAX_IOURING_TIER: Caps the detected io_uring tier at startup ("none", "base", "high",
//     "optional"), to exercise lower-tier fallback paths on modern kernels. Any other value
//     counts as "none". At "none" this engine reports io_uring as unavailable, and the adaptive
//     engine treats io_uring as not viable, so it neither starts on it nor switches to it.
package iouring
