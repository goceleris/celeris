package jwtparse

import "testing"

func FuzzParse(f *testing.F) {
	f.Add("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0In0.abc")
	f.Add("")
	f.Add("...")
	f.Add("a.b.c")
	f.Add("a.b.c.d")
	f.Add("eyJhbGciOiJub25lIn0.eyJzdWIiOiIxMjM0In0.")

	secret := []byte("test-secret-key-32-bytes-long!!!")
	p := NewParser()
	kf := func(_ *Token) (any, error) { return secret, nil }

	f.Fuzz(func(_ *testing.T, s string) {
		// Must never panic.
		_, _ = p.Parse(s, kf)
	})
}
