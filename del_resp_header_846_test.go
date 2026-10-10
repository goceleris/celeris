package celeris

import "testing"

// TestDelRespHeaderClearsTheVacatedSlot846: delRespHeader shifts the later
// headers down and must not leave a copy of the last one past the new length.
// reset clears up to len only, so a stale slot would keep pointing into
// whatever the header's strings referenced (a request buffer, on the native
// engines) until the next request overwrote it.
func TestDelRespHeaderClearsTheVacatedSlot846(t *testing.T) {
	s, _ := newTestStream("GET", "/")
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)

	c.SetHeader("a", "1")
	c.SetHeader("b", "2")
	c.SetHeader("c", "3")
	c.delRespHeader("a")

	if got := c.respHeaders; len(got) != 2 || got[0] != [2]string{"b", "2"} || got[1] != [2]string{"c", "3"} {
		t.Fatalf("headers after delete: %v; want [b=2 c=3]", got)
	}
	if tail := c.respHeaders[:3][2]; tail != ([2]string{}) {
		t.Fatalf("slot past the end holds %v after the delete; want it cleared", tail)
	}
	c.delRespHeader("missing") // no header: nothing changes
	if len(c.respHeaders) != 2 {
		t.Fatalf("deleting a missing header changed the list: %v", c.respHeaders)
	}
}
