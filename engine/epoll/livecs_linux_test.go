//go:build linux

package epoll

// liveEntry returns the connState at index i of l.liveConns, or nil when the
// entry no longer resolves to one.
func liveEntry(l *Loop, i int) *connState {
	fd := l.liveConns[i]
	if fd < 0 || fd >= len(l.conns) {
		return nil
	}
	return l.conns[fd]
}
