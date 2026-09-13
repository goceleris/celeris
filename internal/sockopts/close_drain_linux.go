//go:build linux && !celeris_closeprobe

package sockopts

// CloseDrain is the middle step of the SHUT_WR -> drain -> close(2)
// sequence both loop engines run on a graceful close. In a release build it
// is exactly [DrainRecvBuffer]; site and raddr are unused.
//
// Build with -tags celeris_closeprobe for the measurement variant
// (celeris#583), which can skip the drain (the drain-off arm) and report one
// CLOSE-PROBE record per close. Keeping that variant behind a build tag means
// a release binary carries no environment read and no ioctl on the close
// path.
func CloseDrain(fd int, _ string, _ string) int {
	return DrainRecvBuffer(fd)
}
