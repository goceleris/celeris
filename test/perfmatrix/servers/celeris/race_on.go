//go:build race

package celeris

import "time"

// bindDeadline is bumped to 30 s under -race because the race detector adds
// roughly 3-5x CPU overhead per worker init. With dual-engine adaptive cells
// (12-16 iouring workers + 12-16 epoll loops) on slow ARM cores, the
// compounded init can land just past 5 s and cause spurious "did not bind"
// fail-fasts that are not actually engine bugs — observed on msr1 (kernel
// 7.0.0-14 aarch64) at roughly 1 cell per 80 in the strict matrix.
const bindDeadline = 30 * time.Second
