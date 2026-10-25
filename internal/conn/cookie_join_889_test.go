package conn

import (
	"runtime"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h1"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#889 item 16: an HTTP/1.1 request that sends several Cookie lines
// reaches the handler as one cookie field joined with "; ", as an HTTP/2
// request's split cookie fields do (celeris#944), so Header("cookie"),
// Cookie, and every key built from them (singleflight, cache VaryHeaders)
// see the same value.

func cookieFields889(hs [][2]string) []string {
	var out []string
	for _, h := range hs {
		if h[0] == "cookie" {
			out = append(out, h[1])
		}
	}
	return out
}

func TestMaterializeHeadersJoinsCookieLines889(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  []string
	}{
		{"none", nil, nil},
		{"one", []string{"a=1; b=2"}, []string{"a=1; b=2"}},
		{"two", []string{"lang=en", "sid=ALICE"}, []string{"lang=en; sid=ALICE"}},
		{"three", []string{"a=1", "b=2", "c=3"}, []string{"a=1; b=2; c=3"}},
		{"empty-first", []string{"", "sid=ALICE"}, []string{"; sid=ALICE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := stream.NewH1Stream(1)
			defer s.Release()
			s.Method, s.Path, s.Scheme, s.Authority = "GET", "/", "http", "x"
			raw := [][2][]byte{{[]byte("Host"), []byte("x")}}
			for _, l := range tc.lines {
				raw = append(raw, [2][]byte{[]byte("Cookie"), []byte(l)})
				raw = append(raw, [2][]byte{[]byte("X-Other"), []byte("1")})
			}
			s.LazyRawHeaders = raw
			s.MaterializeHeaders()
			got := cookieFields889(s.Headers)
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Fatalf("cookie fields = %q, want %q (headers %q)", got, tc.want, s.Headers)
			}
			others := 0
			for _, h := range s.Headers {
				if h[0] == "x-other" {
					others++
				}
			}
			if others != len(tc.lines) {
				t.Errorf("%d x-other fields, want %d: only cookie fields are joined", others, len(tc.lines))
			}
		})
	}
}

func TestH2CUpgradeHeadersJoinCookieLines889(t *testing.T) {
	var req h1.Request
	req.RawHeaders = [][2][]byte{
		{[]byte("Host"), []byte("x")},
		{[]byte("Connection"), []byte("Upgrade, HTTP2-Settings")},
		{[]byte("Cookie"), []byte("lang=en")},
		{[]byte("Upgrade"), []byte("h2c")},
		{[]byte("Cookie"), []byte("sid=ALICE")},
		{[]byte("HTTP2-Settings"), []byte("AAMAAABkAAQAAP__")},
	}
	got := cookieFields889(copyH2CHeaders(&req))
	if len(got) != 1 || got[0] != "lang=en; sid=ALICE" {
		t.Fatalf("upgrade request cookie fields = %q, want one \"lang=en; sid=ALICE\"", got)
	}
}

// BenchmarkMaterializeHeaders889 is Stream.MaterializeHeaders for a browser-like
// request of ten headers (the cookie join is one string comparison per header)
// with one Cookie line, and with the same cookies in two lines (the join).
func BenchmarkMaterializeHeaders889(b *testing.B) {
	base := [][2][]byte{
		{[]byte("Host"), []byte("example.com")},
		{[]byte("User-Agent"), []byte("Mozilla/5.0")},
		{[]byte("Accept"), []byte("text/html")},
		{[]byte("Accept-Language"), []byte("en")},
		{[]byte("Accept-Encoding"), []byte("gzip, deflate, br")},
		{[]byte("Connection"), []byte("keep-alive")},
		{[]byte("Cache-Control"), []byte("no-cache")},
		{[]byte("Referer"), []byte("https://example.com/")},
	}
	for _, tc := range []struct {
		name    string
		cookies []string
	}{
		{"one-line", []string{"theme=dark; lang=en; sid=alice"}},
		{"two-lines", []string{"theme=dark; lang=en", "sid=alice"}},
	} {
		raw := append([][2][]byte(nil), base...)
		for _, c := range tc.cookies {
			raw = append(raw, [2][]byte{[]byte("Cookie"), []byte(c)})
		}
		b.Run(tc.name, func(b *testing.B) {
			s := stream.NewH1Stream(1)
			defer s.Release()
			b.ReportAllocs()
			for b.Loop() {
				stream.ResetH1Stream(s)
				s.Headers = s.Headers[:0]
				s.Method, s.Path, s.Scheme, s.Authority = "GET", "/", "http", "example.com"
				s.LazyRawHeaders = raw
				s.MaterializeHeaders()
			}
		})
	}
}

// The join is linear in the request, however many Cookie lines it has: a line
// as small as "a=b" must not make joining quadratic (the HTTP/2 join gathers
// into one buffer for the same reason, celeris#944). The std engine does not
// bound the number of fields, only their bytes, so this is reachable from the
// network. 5000 lines of 3 bytes join to ~25 KB; copying the accumulated
// value for each line would allocate ~60 MB.
func TestCookieJoinIsLinear889(t *testing.T) {
	const lines = 5000
	raw := make([][2][]byte, 0, lines)
	for range lines {
		raw = append(raw, [2][]byte{[]byte("Cookie"), []byte("a=b")})
	}
	const joinedLen = lines*3 + (lines-1)*2
	const budget = 1 << 20 // generous: the linear join allocates a few times the joined length
	measure := func(name string, f func() int) {
		t.Helper()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		n := f()
		runtime.ReadMemStats(&after)
		if n != joinedLen {
			t.Fatalf("%s: joined cookie is %d bytes, want %d", name, n, joinedLen)
		}
		if got := after.TotalAlloc - before.TotalAlloc; got > budget {
			t.Errorf("%s: joining %d cookie lines allocated %d bytes (joined value %d): the join is not linear", name, lines, got, joinedLen)
		}
	}
	measure("MaterializeHeaders", func() int {
		s := stream.NewH1Stream(1)
		defer s.Release()
		s.Method, s.Path, s.Scheme, s.Authority = "GET", "/", "http", "x"
		s.LazyRawHeaders = raw
		s.MaterializeHeaders()
		return len(cookieFields889(s.Headers)[0])
	})
	measure("h2c upgrade", func() int {
		var req h1.Request
		req.RawHeaders = raw
		return len(cookieFields889(copyH2CHeaders(&req))[0])
	})
}
