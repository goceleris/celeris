//go:build linux

package iouring

import (
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#884 item 40: hijackConn submits the cancel of the recv still armed on
// the socket once the conn has left the tables, so that the kernel has it
// before the hijacker can read, and ignored the result. A failed enter left
// the cancel in the SQ ring, to go out with the worker's next submit, while the
// hijacker already had the socket and the recv, still armed, could take its
// first bytes. The refusal for a full SQ ring (celeris#869) is placed before
// the conn leaves and does not cover it. EAGAIN and EBUSY mean the kernel took
// nothing (retryPending), so the submit is offered again, a bounded number of
// times; the seam stands in for an enter the kernel refuses.

func TestHijackSubmitIsRetriedWhenTheKernelTakesNothing884(t *testing.T) {
	for _, errno := range []unix.Errno{unix.EAGAIN, unix.EBUSY} {
		t.Run(errno.Error(), func(t *testing.T) {
			f := newMultishotFixture869(t)
			w, cs := f.w, f.cs
			if !w.prepareRecv(cs, cs.buf) || !cs.recvArmed {
				t.Fatalf("multishot recv not armed")
			}
			calls := 0
			w.hijackSubmit = func() (int, error) {
				calls++
				if calls < 3 {
					return 0, errno // the kernel took nothing: the SQE stays pending
				}
				return w.ring.Submit()
			}
			c, err := w.hijackConn(f.fd)
			if err != nil {
				t.Fatalf("hijackConn: %v", err)
			}
			defer func() { _ = c.Close() }()
			if calls < 3 {
				t.Errorf("the submit ran %d times: a submit the kernel refused with %v was not offered again, and the cancel of the armed recv is still in the SQ ring (pending %d) while the hijacker has the socket",
					calls, errno, w.ring.Pending())
			}
			if p := w.ring.Pending(); p != 0 {
				t.Errorf("%d SQEs still pending after a hijack whose submit was retried until it went through", p)
			}
		})
	}
}

// A submit that never goes through must not hold the worker: the retry is
// bounded, and the hijack, whose conn has left the tables, still completes.
func TestHijackSubmitRetryIsBounded884(t *testing.T) {
	f := newMultishotFixture869(t)
	w, cs := f.w, f.cs
	if !w.prepareRecv(cs, cs.buf) || !cs.recvArmed {
		t.Fatalf("multishot recv not armed")
	}
	calls := 0
	w.hijackSubmit = func() (int, error) {
		calls++
		return 0, unix.EBUSY
	}
	c, err := w.hijackConn(f.fd)
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	defer func() { _ = c.Close() }()
	if calls < 2 || calls > 16 {
		t.Errorf("a submit that always fails ran %d times, want a bounded retry (2 to 16)", calls)
	}
}

// Any other error means the kernel consumed the batch (retryPending), so the
// submit is not repeated: offering the SQEs again would submit them twice.
func TestHijackSubmitIsNotRetriedOnOtherErrors884(t *testing.T) {
	f := newMultishotFixture869(t)
	w, cs := f.w, f.cs
	if !w.prepareRecv(cs, cs.buf) || !cs.recvArmed {
		t.Fatalf("multishot recv not armed")
	}
	calls := 0
	w.hijackSubmit = func() (int, error) {
		calls++
		return 0, unix.EINTR
	}
	c, err := w.hijackConn(f.fd)
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	defer func() { _ = c.Close() }()
	if calls != 1 {
		t.Errorf("a submit that failed with EINTR ran %d times, want 1", calls)
	}
}
