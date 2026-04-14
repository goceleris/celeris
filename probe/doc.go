// Package probe provides capability detection for io_uring and epoll.
//
// At engine startup, the celeris io_uring engine calls Detect() to discover
// which kernel features are available (multishot accept/recv, provided
// buffer rings, fixed-file tables, SEND_ZC, etc.) and assigns a Tier from
// the resulting Capabilities. Application code typically does not call
// this package directly; inspect [celeris.Server.EngineInfo] at runtime
// for the active feature set.
package probe
