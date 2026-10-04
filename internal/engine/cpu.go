package engine

import "github.com/goceleris/celeris/internal/cpumon"

// CPUSample is a point-in-time CPU utilization measurement supplied by a
// CPUMonitor. Utilization is a fraction in [0,1]; zero means "no signal" and
// is the documented fallback when system-wide CPU data is unavailable. It is
// an alias of [cpumon.CPUSample].
type CPUSample = cpumon.CPUSample

// CPUMonitor samples CPU utilization. It is the interface accepted by the
// adaptive engine, so a caller can supply its own monitor (or the built-in
// /proc/stat implementation). Implementations must be safe for concurrent
// use: a single monitor may be sampled from more than one goroutine. It is an
// alias of [cpumon.Monitor].
type CPUMonitor = cpumon.Monitor
