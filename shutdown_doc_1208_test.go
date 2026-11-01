package celeris_test

import "testing"

// TestShutdownDocConnShapes1208 pins celeris#1208 on the Server.Shutdown
// godoc: what a connection gets depends on what it has sent when Shutdown
// begins, per engine. The behaviour is pinned by
// test/server/shutdown_conn_shapes_1208_linux_test.go; this is its text. The
// stale sentence promised an answer to a connection that sent nothing, which no
// engine gives, and the older text said nothing of a head in the kernel, which
// std drops and the native engines answer.
func TestShutdownDocConnShapes1208(t *testing.T) {
	doc := docText(t, "server.go", "Server", "Shutdown")
	requireDoc(t, "Server.Shutdown", doc,
		[]string{
			"What a connection gets depends on what it has sent when Shutdown begins",
			"An idle keep-alive connection, which has no request under way, is closed at once on every engine",
			"A request in progress, whose head the server has read, is read on and answered, with Connection: close as above, on every engine",
			"is answered, with Connection: close, on epoll, io_uring and adaptive, as a request in progress is",
			"the head is answered on epoll and dropped on io_uring: the connection is closed without an answer",
			"adaptive does what the engine it is running on does, epoll when it starts",
			"net/http reads the head after Shutdown began and drops the request: the connection is closed without an answer, and reset if the request has a body",
			"A connection that has sent nothing is not answered on any engine, and a request it sends once Shutdown has begun is dropped",
			"closed at once if the engine has accepted it",
			"while it is held back Shutdown neither closes it nor lets a request through: its first request gets a reset",
			"or until net/http closes it for being five seconds old",
			"can hold Shutdown on std for about five seconds",
		},
		[]string{
			"(it is answered close when its first request is)",
			"it is answered close when its first request is",
		})
}
