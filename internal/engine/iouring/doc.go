//go:build linux

// Package iouring implements an asynchronous network I/O engine backed by Linux io_uring.
//
// # Kernel Requirement
//
// The engine needs Linux 5.19 or later. Every cancel it submits (connection close, hijack, the
// accept pause, the WebSocket backpressure pause, the driver unregister and the io_uring→epoll
// hand-off) sets IORING_ASYNC_CANCEL flags, which that release added; an older kernel fails each
// one with -EINVAL and leaves the operation it targets running (celeris#682). New probes for the
// flags and, where the kernel rejects them (or, before 5.19, where the probe gets no answer),
// returns an error that starts "io_uring not available on this system" and names the requirement.
// The adaptive engine then runs on epoll.
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
//     value examined. The on/off A/B of celeris#585 (2026-09-27) kept "auto" on: on neither
//     arch did SEND_ZC carry enough of a benchmark cell's bytes to vote, and the default
//     changes only on a measured loss.
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
