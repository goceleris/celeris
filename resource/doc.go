// Package resource defines server configuration, resource limits, and default
// presets. The top-level celeris.Config is the primary user-facing type; this
// package provides the internal representation used by engine implementations.
//
// Key exported symbols:
//   - [Config] — internal server configuration with [Config.Validate] and
//     [Config.WithDefaults] helpers used by engine implementations.
//   - [Resources] — optional user overrides for buffer counts and concurrency
//     limits; zero values fall back to engine defaults.
//   - [ResolvedResources] — the fully resolved values after applying defaults,
//     user overrides, and hard caps; consumed by engines at startup.
//   - [WorkloadHint] — optional operator hint ([WorkloadLowConcurrency] /
//     [WorkloadHighConcurrency])
//     that influences which I/O backend the adaptive engine starts on.
//   - [NextPowerOf2] — utility used by engine packages to size io_uring rings
//     and similar power-of-two kernel structures.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/configuration
package resource
