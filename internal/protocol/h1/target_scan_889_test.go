package h1

import "testing"

// The target scan reads eight bytes at a time: check it against the
// byte-at-a-time definition for every byte value, in every position of
// targets of every length around the word size, over backgrounds that
// include bytes above 0x7f (which can carry a borrow into the next lane).
func TestIsValidRequestTargetMatchesTheTable889(t *testing.T) {
	ref := func(b []byte) bool {
		for _, c := range b {
			if c < 0x20 || c == 0x7f {
				return false
			}
		}
		return true
	}
	for _, bg := range []byte{'a', 0x21, 0x7e, 0x80, 0xff, 0x20} {
		for n := 0; n <= 40; n++ {
			buf := make([]byte, n)
			for i := range buf {
				buf[i] = bg
			}
			if got, want := isValidRequestTarget(buf), ref(buf); got != want {
				t.Fatalf("bg %#x len %d: got %v want %v", bg, n, got, want)
			}
			for pos := 0; pos < n; pos++ {
				for c := 0; c < 256; c++ {
					buf[pos] = byte(c)
					if got, want := isValidRequestTarget(buf), ref(buf); got != want {
						t.Fatalf("bg %#x len %d pos %d byte %#x: got %v want %v", bg, n, pos, c, got, want)
					}
				}
				buf[pos] = bg
			}
		}
	}
}
