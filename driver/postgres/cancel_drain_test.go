package postgres

import (
	"context"
	"testing"
	"time"
)

// A confirmed cancel whose statement never answers: driveDirect's bounded drain
// ends on the read deadline. The conn must then be failed (discarded), not put
// back into the pool with the cancelled statement's request still pending: the
// late reply of that statement would be dispatched to the next statement on the
// conn (a crossed reply), or the next statement would hang behind it.
func TestDirectDrainTimeoutDiscardsTheConn(t *testing.T) {
	old := cancelDrainTimeout
	cancelDrainTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cancelDrainTimeout = old })

	f := startFakePostmaster(t, 0, "never")
	arm := cancelArms()[0] // pool-direct
	run := arm.open(t, f.addr)
	warmUp(t, run, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-f.slowCh
		cancel()
	}()
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, fakeSlowQuery, false) }()
	select {
	case <-errc:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled call did not return after the drain timeout")
	}
	next := make(chan error, 1)
	go func() { next <- run(context.Background(), fakeNextQuery, false) }()
	select {
	case err := <-next:
		if err != nil {
			t.Fatalf("the next statement failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the next statement hung behind the cancelled statement's request: the conn was reused (%v)", f.events())
	}
	if got := f.sessions(); got != 2 {
		t.Fatalf("sessions = %d, want 2: the conn whose drain timed out was reused with the cancelled statement still pending (%v)", got, f.events())
	}
}
