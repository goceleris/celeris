//go:build !windows

package postgres

// rstAfterCompleteIsClean is whether a reply that was sent in full and followed at
// once by a reset (SetLinger(0), then Close) is still delivered in full: the data
// is in the client's receive buffer when the RST arrives, and the client reads it.
// Linux and macOS keep it. Windows drops the unread receive buffer on a RST, so
// there a "complete reply, then RST" is a real cut and the cells that expect a
// complete result are not run (see connloss_rst_windows_test.go).
const rstAfterCompleteIsClean = true
