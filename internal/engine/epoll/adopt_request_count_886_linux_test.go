//go:build linux

package epoll

import (
	"bufio"
	"strings"
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
// counts one, except one that is an h2c upgrade request (none, as on the
// recv path, item 23), and an adoption with an empty carry counts none, as no
// recv has happened. Each test adopts one conn onto a fresh engine, so the count
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
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	return bufio.NewReader(client)
}

// expectUpgradeAnswered886 reads the 101 of an h2c upgrade off the client: the
// replay has then run to its verdict, so a count it was going to make has been
// made. Without it expectCount886 can return, with 0, before the replay ran.
func expectUpgradeAnswered886(t *testing.T, br *bufio.Reader) {
	t.Helper()
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		t.Fatalf("the upgrade request was answered %q (err %v), want a 101", status, err)
	}
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

// A carry that is an h2c upgrade request counts none, as the recv path counts
// none for the request that upgrades the conn (it returns on the upgrade
// verdict before it counts) and as an HTTP/2 conn counts no request on
// either path (celeris#886 item 23: the replay counted one, the recv path
// none, and the rule picked is the recv path's, so that BytesPerReq does not
// move by one request per upgraded conn depending on how the conn arrived).
func TestEpollAdoptRequestCountUpgradeCarry886(t *testing.T) {
	e, _ := start908(t, false, true) // EnableH2Upgrade: the carry is a real upgrade
	br := adoptCount886(t, e, h2cUpgradeHead908())
	expectUpgradeAnswered886(t, br)
	expectCount886(t, e, 0)
}

func TestEpollAdoptRequestCountUpgradeCarryAsync886(t *testing.T) {
	e, _ := start908(t, true, true) // EnableH2Upgrade: the carry is a real upgrade
	br := adoptCount886(t, e, h2cUpgradeHead908())
	expectUpgradeAnswered886(t, br)
	expectCount886(t, e, 0)
}
