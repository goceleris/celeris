// Package probe provides kernel capability detection for the io_uring engine.
//
// At engine startup, [Probe] (or [ProbeWith] for testing) discovers which
// kernel features are available — multishot accept/recv, provided buffer
// rings, fixed-file tables, SEND_ZC, SQPOLL, and more — and returns an
// [engine.CapabilityProfile] that includes the selected [engine.Tier].
// Application code typically does not call this package directly; inspect
// [celeris.Server.EngineInfo] at runtime for the active feature set.
// [DiagnosticReport] and [FormatDiagnostic] can surface the profile as
// human-readable text for troubleshooting.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/engines
package probe
