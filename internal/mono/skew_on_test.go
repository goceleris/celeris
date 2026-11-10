//go:build celeris_monoskew

package mono

import (
	"os"
	"testing"
	"time"
)

// The canary's own test: the clock reads CELERIS_MONO_SKEW away from the wall,
// in every form (Now, FromWall, FromTime), and the conversions that cross to
// the wall (Time) cancel it. A site that did not go through the package reads
// the wall and is an hour off from every stamp of the package.
func TestSkewMovesEveryForm(t *testing.T) {
	if os.Getenv("CELERIS_MONO_SKEW") == "" {
		t.Fatal("built with -tags celeris_monoskew, which is for canary runs only: set CELERIS_MONO_SKEW (for example 3600s or -1h)")
	}
	want := skew
	if want == 0 {
		t.Fatal("CELERIS_MONO_SKEW is set and parses to 0")
	}
	if d := Now() - time.Now().UnixNano() - want; abs(d) > tol {
		t.Errorf("Now is %v from wall+skew", time.Duration(d))
	}
	if d := nowMonotonic() - time.Now().UnixNano() - want; abs(d) > tol {
		t.Errorf("the monotonic form is %v from wall+skew", time.Duration(d))
	}
	if d := FromWall(time.Now().UnixNano()+int64(time.Second)) - Now() - int64(time.Second); abs(d) > tol {
		t.Errorf("FromWall(now+1s) is %v from Now()+1s", time.Duration(d))
	}
	if d := FromTime(time.Now()) - Now(); abs(d) > tol {
		t.Errorf("FromTime(now) is %v from Now()", time.Duration(d))
	}
	// The wall instant of a stamp is the real one: the skew cancels.
	if d := abs(Time(Now()).UnixNano() - time.Now().UnixNano()); d > tol {
		t.Errorf("Time(Now()) is %v from the real wall", time.Duration(d))
	}
	if abs(Now()-time.Now().UnixNano()) < int64(time.Minute) {
		t.Error("a wall reading and Now are within a minute: a site that skipped the package would go unseen")
	}
}
