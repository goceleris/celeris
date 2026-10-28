// Package tcpack answers one question for the engines' idle clock of an HTTP/2
// connection (celeris#889): is a count of 0 in a socket's tcp_info bytes_acked a
// count, or the sign of a stack that does not fill the field?
//
// On a Linux kernel since 4.1 the field is filled, and a connection that has
// just been accepted reads 0 because its peer has acknowledged no byte of data:
// the idle clock's progress cap must treat that as "no progress", or a peer with
// its own TCP stack that never acknowledges data (or advertises a zero window
// from the start) holds a conn with output for ever. On a stack that does not
// fill it (gVisor's netstack, as far as is known) a 0 says nothing, and the cap
// must not close a conn for it. The two are told apart once, by asking this
// host's stack: a loopback connection, one byte written, the count read from the
// writer's side. The probe runs in the background (the engines start it when they
// are built) and is never waited for: until it has said yes, a 0 is "no count".
// The answer is latched true as soon as the probe says so or any socket of the
// process has reported a count above 0.
package tcpack
