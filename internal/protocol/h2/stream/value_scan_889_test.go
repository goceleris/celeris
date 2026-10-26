package stream

import "testing"

// hasForbiddenValueByte reads eight bytes at a time: check it against the
// byte-at-a-time definition for every byte value, in every position of values
// of every length around the word size, over backgrounds that include bytes
// above 0x7f.
func TestHasForbiddenValueByteMatchesTheDefinition889(t *testing.T) {
	ref := func(s string) bool {
		for i := 0; i < len(s); i++ {
			if c := s[i]; c == 0 || c == '\n' || c == '\r' {
				return true
			}
		}
		return false
	}
	for _, bg := range []byte{'a', 0x0e, 0x0f, 0x01, 0x09, 0x80, 0xff, 0x20} {
		for n := 0; n <= 40; n++ {
			buf := make([]byte, n)
			for i := range buf {
				buf[i] = bg
			}
			if got, want := hasForbiddenValueByte(string(buf)), ref(string(buf)); got != want {
				t.Fatalf("bg %#x len %d: got %v want %v", bg, n, got, want)
			}
			for pos := 0; pos < n; pos++ {
				for c := 0; c < 256; c++ {
					buf[pos] = byte(c)
					if got, want := hasForbiddenValueByte(string(buf)), ref(string(buf)); got != want {
						t.Fatalf("bg %#x len %d pos %d byte %#x: got %v want %v", bg, n, pos, c, got, want)
					}
				}
				buf[pos] = bg
			}
		}
	}
}
