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
	requireDocTagged(t, "celeris#1208", "Server.Shutdown", doc,
		[]string{
			"What a connection gets depends on what it has sent when Shutdown begins",
			"measured on arm64, in docker with 2 CPUs, on a 7.0 kernel with io_uring's high tier",
			"is closed at once by std and epoll, and by an io_uring worker with no response going out",
			"so its idle connections are closed at 250 ms, and a request one of them sends before then is answered with Connection: close",
			"A request in progress, whose head the server has read, is read on and answered, with Connection: close as above, on every engine",
			"is answered, with Connection: close, on epoll, and on io_uring while its worker has a response going out or a request in progress when it sees the Shutdown",
			"An io_uring worker with neither exits at once, and such a connection is closed without an answer",
			"net/http reads the head after Shutdown began and drops the request: the connection is closed without an answer, and reset if body bytes are still unread",
			"A connection that has sent nothing is not answered by std, or by epoll and an io_uring worker with no response going out",
			"its first request gets a reset",
			"An io_uring worker that has a response going out keeps accepting for its first 250 ms",
			"answers, with Connection: close, a first request that arrives before then, on an accepted connection or a held-back one",
			"or until net/http closes it for being five seconds old",
			"can hold Shutdown on std for about five seconds",
		},
		[]string{
			"(it is answered close when its first request is)",
			"it is answered close when its first request is",
		})
}
