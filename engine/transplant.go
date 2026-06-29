package engine

// Carryover is the minimal per-connection state transferred when a live
// connection is transplanted from one engine to another at a request boundary
// (#383). At a clean HTTP/1 boundary the connection is equivalent to a freshly
// accepted socket, so only cosmetic/identity state needs to move; any in-flight
// request bytes remain in the kernel socket receive buffer and are recv'd by
// the destination engine after it arms its first read.
type Carryover struct {
	// RemoteAddr is the peer address string captured by the source engine, so
	// the destination need not re-Getpeername the fd.
	RemoteAddr string

	// Buffered holds bytes the source engine had already read off the socket but
	// not yet processed — buffered pipelined NEXT-request bytes that sat in the
	// HTTP/1 parser at a clean request boundary (#383). The destination replays
	// them through its parser before arming its first read. Empty in the common
	// case (the socket was fully drained at the boundary). Only ever carries
	// next-request bytes — the source guarantees no in-progress request remains
	// (AtRequestBoundary), so a fresh parser can consume them correctly.
	Buffered []byte
}

// TransplantTarget is implemented by an engine that can adopt a live, already
// connected file descriptor handed off from another engine mid-flight (#383).
//
// Protocol: the SOURCE engine detaches the fd from its own event loop FIRST
// (EPOLL_CTL_DEL / ring-cancel + remove from its conn table), so the fd is owned
// by no engine at the moment AdoptConn is called; incoming bytes simply queue in
// the kernel socket receive buffer until the destination arms its read. Only
// then does it call AdoptConn.
//
// Implementations must be safe to call from any goroutine; the actual attach is
// deferred onto the destination engine's own worker thread (where SQE/epoll
// submission is safe).
type TransplantTarget interface {
	// AdoptConn takes ownership of fd — a connected, non-blocking real socket
	// sitting at an HTTP/1 request boundary — and begins serving it. The caller
	// must have already removed fd from its own engine. On a returned error the
	// caller still owns fd and is responsible for closing it.
	AdoptConn(fd int, carry Carryover) error
}
