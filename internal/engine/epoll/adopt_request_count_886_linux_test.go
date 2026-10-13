//go:build linux

package epoll

import (
	"bufio"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#886 item 5 (and #899 item 1): RequestCount after an adoption.
// Engines count a request per recv event of the recv path (a batch counter
// the loop flushes each iteration); an adopted conn's carried bytes are
// replayed in place of a recv, and the replay is that recv event. The rule
// these tests pin, for every replay path (sync, async served inline, async
// promoted, a replay that ends on a verdict): a replay of a non-empty carry
// counts one, and an adoption with an empty carry counts none, as no recv
// has happened. Each test adopts one conn onto a fresh engine, so the count
// after it is the count of that adoption.

// expectCount886 waits until RequestCount reaches want and then holds for
// settle to see that nothing counts it again: the batch is flushed on the
// loop's next iteration, so the first reading may be early, and a double
// count would arrive after it.
func expectCount886(t *testing.T, e *Engine, want uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	got := e.Metrics().RequestCount
	for got < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		got = e.Metrics().RequestCount
	}
	if got != want {
		t.Fatalf("RequestCount after the adoption = %d, want %d", got, want)
	}
	// Hold: a later flush (a second increment) would move it.
	for end := time.Now().Add(400 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if got = e.Metrics().RequestCount; got != want {
			t.Fatalf("RequestCount settled at %d after the adoption, want %d", got, want)
		}
	}
}

// adoptCount886 adopts a conn carrying carried onto e.
func adoptCount886(t *testing.T, e *Engine, carried string) (clientReader *bufio.Reader) {
	t.Helper()
	client, fd := adoptable543(t)
	carry := engine.Carryover{RemoteAddr: client.LocalAddr().String()}
	if carried != "" {
		carry.Buffered = []byte(carried)
	}
	if err := e.AdoptConn(fd, carry); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("AdoptConn: %v", err)
	}
	return bufio.NewReader(client)
}

func TestEpollAdoptRequestCountSync886(t *testing.T) {
	e, _ := start908(t, false, false)
	adoptCount886(t, e, carryGood908)
	expectCount886(t, e, 1)
}

func TestEpollAdoptRequestCountSyncVerdict886(t *testing.T) {
	e, _ := start908(t, false, false)
	adoptCount886(t, e, carryBad908)
	expectCount886(t, e, 1)
}

func TestEpollAdoptRequestCountAsyncInline886(t *testing.T) {
	e, _ := start908(t, true, false)
	adoptCount886(t, e, carryGood908)
	expectCount886(t, e, 1)
}

func TestEpollAdoptRequestCountAsyncVerdict886(t *testing.T) {
	e, _ := start908(t, true, false)
	adoptCount886(t, e, carryBad908)
	expectCount886(t, e, 1)
}

func TestEpollAdoptRequestCountAsyncPromoted886(t *testing.T) {
	h := newCarry543Handler()
	e := start543(t, true, h)
	t.Cleanup(func() { close(h.release) })
	adoptCount886(t, e, "GET /block HTTP/1.1\r\nHost: x\r\n\r\n")
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the carried /block request never reached its handler")
	}
	expectCount886(t, e, 1)
}

func TestEpollAdoptRequestCountEmptyCarry886(t *testing.T) {
	e, _ := start908(t, false, false)
	adoptCount886(t, e, "")
	expectCount886(t, e, 0)
}

// A carry that is an h2c upgrade request counts one like any other non-empty
// carry. The recv path does not: it returns on the upgrade verdict before it
// counts, so a conn that upgraded on a recv has counted none (celeris#886
// item 5, open question in the PR that added these). The tests pin the
// replay's answer so that it cannot drift unnoticed, whichever way the
// question is settled.
func TestEpollAdoptRequestCountUpgradeCarry886(t *testing.T) {
	e, _ := start908(t, false, true) // EnableH2Upgrade: the carry is a real upgrade
	adoptCount886(t, e, h2cUpgradeHead908())
	expectCount886(t, e, 1)
}

func TestEpollAdoptRequestCountUpgradeCarryAsync886(t *testing.T) {
	e, _ := start908(t, true, true) // EnableH2Upgrade: the carry is a real upgrade
	adoptCount886(t, e, h2cUpgradeHead908())
	expectCount886(t, e, 1)
}
