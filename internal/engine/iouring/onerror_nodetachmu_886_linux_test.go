//go:build linux

package iouring

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// celeris#886 item 9. OnError delivery on a conn with no detachMu differs by
// engine: epoll calls H1State.OnError on a processing error whenever one is
// registered (endH1Verdict), io_uring only under detachMu, so only for a conn
// that has one. A sync conn gets a detachMu at Detach; a handler that
// registers a callback (Context.SetWSErrorHandler, exported) and does not
// Detach, or whose Detach faulted and was rolled back (the rollback puts
// detachMu back to nil and leaves the callback), is such a conn. epoll's side
// is pinned by TestEpollRecvPathOnErrorSync908 and
// TestEpollAdoptCarriedOnErrorSync908; these are the other side. They pin what
// io_uring does today, and are the ones to flip if the rule is unified.

func TestIouringRecvPathOnErrorSyncNoDetachMuIsDropped886(t *testing.T) {
	e, h := start908(t, false, false)
	client, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("GET /onerr HTTP/1.1\r\nHost: x\r\n\r\n" + carryBad908)); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(client)
	for i, want := range []int{200, 400} {
		code, _, err := readBody543(client, br, 3*time.Second)
		if err != nil || code != want {
			t.Fatalf("response %d: got %d, err %v, want %d", i+1, code, err, want)
		}
	}
	// The engine calls OnError (if it does) before it closes the conn, so once
	// the close is seen the callback has had its chance.
	if !hasEOF908(client, br, 3*time.Second) {
		t.Fatal("the conn stayed open after the 400")
	}
	select {
	case err := <-h.errs:
		t.Errorf("io_uring delivered %v to the OnError of a sync conn with no detachMu: the rule changed (epoll does deliver); update item 886.9's comments and this pin", err)
	default:
	}
}

func TestIouringAdoptCarriedOnErrorSyncNoDetachMuIsDropped886(t *testing.T) {
	e, h := start908(t, false, false)
	runCarryReplies908(t, e.AdoptConn, "GET /onerr HTTP/1.1\r\nHost: x\r\n\r\n"+carryBad908, []step908{{200, "/onerr"}, {400, ""}})
	select {
	case err := <-h.errs:
		t.Errorf("io_uring delivered %v to the OnError of a sync conn with no detachMu on the replay: the rule changed (epoll does deliver); update item 886.9's comments and this pin", err)
	default:
	}
}
