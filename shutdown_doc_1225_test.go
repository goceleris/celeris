package celeris_test

import (
	"os"
	"strings"
	"testing"
)

// TestShutdownDocPartialHead1225 pins celeris#1225 on the Server.Shutdown godoc
// and the README: a connection that has begun a request head and not finished
// it (a fragment, or a bare CRLF) is a request in progress on epoll and io_uring,
// so Shutdown waits for it, up to the limits the text names, and a client can
// hold a graceful shutdown that long; net/http does not count such a connection
// the same way. The behaviour is pinned per engine by
// test/server/shutdown_partial_head_1225_linux_test.go; this is its text. The
// stale sentence promised the wait "until the request completes or the budget
// ends", which leaves out the two limits that end it earlier, and the premise
// the issue was filed on, that net/http holds a connection with a partial head
// as well, is false for a keep-alive connection (it closes it with the idle
// ones).
func TestShutdownDocPartialHead1225(t *testing.T) {
	doc := docText(t, "server.go", "Server", "Shutdown")
	requireDocTagged(t, "celeris#1225", "Server.Shutdown", doc,
		[]string{
			"While a connection that has begun a head is open (the bullet below), epoll closes the idle connections of the loop that serves it at about 250 ms, the gate's floor, where a loop without one closes its idle connections at once, and io_uring closes its idle connections at about 250 ms",
			"A head begun and not completed, a fragment or a bare CRLF, is a request in progress on epoll and io_uring (celeris#1225), whether the connection was idle after a response or is new",
			"does not close the connection as it closes an idle one",
			"Shutdown waits until the head completes (the request is then answered, with Connection: close, and the wait ends), until a limit of the connection ends it, or until ctx is done",
			"The limits are [Config.ReadHeaderTimeout], 10 s by default, counted from the head's first byte (for a new connection, from the engine's accept of it, which TCP_DEFER_ACCEPT, on by default, puts at that byte) and not extended by the bytes that follow",
			"[Config.WriteTimeout], from the start of the wait as above; when one of them ends the wait, Shutdown returns nil if that was all it waited for",
			"A client can therefore hold a graceful shutdown for as long as the shortest of those by sending a bare CRLF or one byte of a head, and the bound the caller sets is ctx's deadline: when it comes before those limits, Shutdown returns at it, with ctx's error, or with nil when the engine's drain, which ends at that same deadline, lets Listen return before ctx's timer fires; the engine closes the connection, which gets its FIN, on epoll, io_uring and adaptive",
			"a drain that ctx's deadline ends can finish at that same instant, and Shutdown then returns nil: nil at the deadline does not mean nothing was cut, celeris#1225",
			"net/http's Shutdown also waits for connections that are not idle until ctx is done, but it does not count this one as the native engines do",
			"so std closes a keep-alive connection that has begun a head, or sent a bare CRLF, with the idle ones, without an answer, and Shutdown returns at once",
			"A new connection that has begun its first head is not idle for net/http until it is about five seconds old",
			"when Shutdown returns ctx's error and leaves the connection open",
		},
		[]string{
			"it holds Shutdown until the request completes or the budget ends, on epoll and io_uring alike",
		})

	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := normaliseDoc(string(b))
	for _, s := range []string{
		"A connection that has begun a request head and not finished it (a fragment, or a bare CRLF) counts as a request in progress on epoll and io_uring",
		"`ReadHeaderTimeout` (10 s by default, from the head's first byte, or for a new connection from its accept)",
		"or until the context passed to `Shutdown` is done, so a client can hold a graceful shutdown for that long and the caller's bound is the context's deadline",
		"std closes a keep-alive connection in that state with the idle ones, and waits for a new connection's first head as for a silent one",
	} {
		if !strings.Contains(readme, s) {
			t.Errorf("README lacks %q (celeris#1225)", s)
		}
	}
}
