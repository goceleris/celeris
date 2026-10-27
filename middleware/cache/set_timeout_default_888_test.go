package cache

import (
	"testing"
	"time"
)

// A Config that leaves SetTimeout alone still bounds the Set: the context the
// store is given carries a deadline, whatever the default is (celeris#888 item
// 30). It uses only API that main has, so it is red there: main hands the
// store the request's own context, which never ends on a native HTTP/1 request.
// The value of the default is TestSetTimeoutZeroSelectsTheDefault888's.
func TestUnsetSetTimeoutBoundsTheStoresContext888(t *testing.T) {
	kv := newHung888(false)
	t.Cleanup(func() { close(kv.release) })
	mw := New(Config{Store: kv, TTL: time.Minute})
	_ = get888("/k", mw, okHandler888("v"))
	waitClosed888(t, "the Set to start", kv.entered, 5*time.Second)
	if kv.deadline.Load() == nil {
		t.Error("the store's Set was given a context without a deadline: a Set that never returns holds the leader for ever")
	}
}
