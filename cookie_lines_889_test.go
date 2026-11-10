package celeris

import (
	"errors"
	"testing"
)

// Tests for celeris#889 item 16. A request that sends several Cookie lines
// reaches the handler as one cookie field joined with "; " (the engines join
// HTTP/1 lines as HTTP/2 joins its split fields at HPACK decode, celeris#944;
// see internal/conn/cookie_join_889_test.go and the every-engine test in
// test/server), so Context.Cookie finds every cookie, the earlier one winning
// when a name repeats, and Header("cookie") returns them all.

func TestCookieFindsEveryCookieOfAJoinedField889(t *testing.T) {
	s, _ := newTestStream("GET", "/cookie")
	s.Headers = append(s.Headers, [2]string{"cookie", "theme=dark; lang=en; sid=alice; ; ;; trace=t1;novalue; last=z "})
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)

	for name, want := range map[string]string{"theme": "dark", "lang": "en", "sid": "alice", "trace": "t1", "last": "z "} {
		got, err := c.Cookie(name)
		if err != nil || got != want {
			t.Errorf("Cookie(%q) = %q, %v; want %q, nil", name, got, err, want)
		}
	}
	for _, name := range []string{"novalue", "missing"} {
		if _, err := c.Cookie(name); !errors.Is(err, ErrNoCookie) {
			t.Errorf("Cookie(%q) err = %v, want ErrNoCookie", name, err)
		}
	}
}

func TestCookieDuplicateNameFirstWins889(t *testing.T) {
	s, _ := newTestStream("GET", "/cookie")
	s.Headers = append(s.Headers, [2]string{"cookie", "sid=first; sid=second"})
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)
	if got, err := c.Cookie("sid"); err != nil || got != "first" {
		t.Fatalf("Cookie(sid) = %q, %v; want first", got, err)
	}
}

// BenchmarkContextCookie889 is a first Cookie lookup (the parse of the
// cookie field) for a field of three cookies.
func BenchmarkContextCookie889(b *testing.B) {
	s, _ := newTestStream("GET", "/cookie")
	s.Headers = append(s.Headers, [2]string{"cookie", "session=abc123; theme=dark; lang=en"})
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)
	b.ReportAllocs()
	for b.Loop() {
		c.cookieCached = false
		c.cookieCache = c.cookieCache[:0]
		_, _ = c.Cookie("lang")
	}
}
