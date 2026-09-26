//go:build linux

package epoll

// liveEntry returns the connState at index i of l.liveConns, or nil when the
// entry no longer resolves to one.
func liveEntry(l *Loop, i int) *connState {
	return l.liveConns[i]
}
