//go:build linux

package epoll

import "testing"

// The epoll half of the celeris#844 rig (async_abort_844_linux_test.go is the
// same file on both engines).

const (
	engineName844 = "epoll"
	pkgDir844     = "epoll"
)

func tier844(*Engine) string { return "n/a" }

// parkRouteReachable844: epoll's park loop runs no application code under
// asyncInMu (askAtPark is engine code), so there is no route-resolver arm.
func parkRouteReachable844(*Engine) (bool, string) {
	return false, "epoll's park loop runs only engine code under asyncInMu (askAtPark)"
}

func unavailable844(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatalf(format, args...) // not a skip: a skip would take the witness out of CI silently
}
