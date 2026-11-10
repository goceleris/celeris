// Package sockoptstest reads, from a test, the socket options of the
// server-side socket a celeris engine accepted, so that a test can say which
// options an engine applied to it (celeris#1036: SO_BUSY_POLL is
// opt-in). The helpers are Linux-only; the package holds nothing else on
// other platforms.
package sockoptstest
