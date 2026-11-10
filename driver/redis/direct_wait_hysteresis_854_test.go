package redis

import (
	"testing"
	"time"
)

// TestDirectBlockNext854 pins the hysteresis: on above 300 us, off below 150 us,
// and unchanged in between whichever way the conn arrived there.
func TestDirectBlockNext854(t *testing.T) {
	us := func(n int) int64 { return int64(time.Duration(n) * time.Microsecond) }
	for _, tc := range []struct {
		on    bool
		ewma  int64
		want  bool
		label string
	}{
		{false, us(0), false, "off, idle"},
		{false, us(300), false, "off, at the on threshold"},
		{false, us(301), true, "off, above"},
		{false, us(200), false, "off, between"},
		{true, us(200), true, "on, between"},
		{true, us(150), true, "on, at the off threshold"},
		{true, us(149), false, "on, below"},
		{true, us(600), true, "on, slow"},
	} {
		if got := directBlockNext(tc.on, tc.ewma); got != tc.want {
			t.Errorf("%s: directBlockNext(%v, %d) = %v, want %v", tc.label, tc.on, tc.ewma, got, tc.want)
		}
	}
}

// TestObserveDirectWait854: the average is seeded by the first sample, moves a
// quarter of the way to each later one, and drives the switch with hysteresis.
func TestObserveDirectWait854(t *testing.T) {
	var c redisConn
	c.observeDirectWait(2 * time.Millisecond)
	if c.directWaitEWMA != int64(2*time.Millisecond) || !c.directBlockOn {
		t.Fatalf("after a 2 ms sample: ewma=%d on=%v", c.directWaitEWMA, c.directBlockOn)
	}
	c.observeDirectWait(200 * time.Microsecond)
	if want := int64(2*time.Millisecond) - (int64(2*time.Millisecond)-int64(200*time.Microsecond))/4; c.directWaitEWMA != want {
		t.Fatalf("ewma = %d, want %d", c.directWaitEWMA, want)
	}
	for i := 0; i < 40 && c.directBlockOn; i++ {
		c.observeDirectWait(50 * time.Microsecond)
	}
	if c.directBlockOn || c.directWaitEWMA >= int64(directBlockOffBelow) {
		t.Fatalf("still on after fast samples: ewma=%d", c.directWaitEWMA)
	}
	c.observeDirectWait(250 * time.Microsecond) // between: stays off
	if c.directBlockOn {
		t.Fatalf("switched on between the thresholds: ewma=%d", c.directWaitEWMA)
	}
}

// TestDirectWaitLatch854 documents a property of the hysteresis band: a conn
// whose steady first-chunk wait sits between the thresholds (here 200 us) stays
// in whichever state it was in, so one spike over the on-threshold (a GC pause,
// a load burst) turns blocking on and it stays on, even though the steady wait
// would not have switched it on. The band's width is what keeps a conn from
// flapping; the cost is that a conn can latch. The cluster A/B records the
// average's distribution to say whether that matters (celeris#854).
func TestDirectWaitLatch854(t *testing.T) {
	var c redisConn
	for i := 0; i < 50; i++ {
		c.observeDirectWait(200 * time.Microsecond)
	}
	if c.directBlockOn {
		t.Fatalf("steady 200 us switched blocking on: ewma=%d", c.directWaitEWMA)
	}
	c.observeDirectWait(2 * time.Millisecond) // one spike, weight 1/4: 200 -> 650 us
	if !c.directBlockOn {
		t.Fatalf("a 2 ms spike did not switch blocking on: ewma=%d", c.directWaitEWMA)
	}
	for i := 0; i < 50; i++ {
		c.observeDirectWait(200 * time.Microsecond)
	}
	if !c.directBlockOn {
		t.Fatalf("steady 200 us switched blocking off again: ewma=%d (the band latches)", c.directWaitEWMA)
	}
	for i := 0; i < 50 && c.directBlockOn; i++ {
		c.observeDirectWait(100 * time.Microsecond)
	}
	if c.directBlockOn {
		t.Fatalf("steady 100 us did not switch blocking off: ewma=%d", c.directWaitEWMA)
	}
}
