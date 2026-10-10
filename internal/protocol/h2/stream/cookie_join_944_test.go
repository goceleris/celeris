package stream

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// celeris#944: a client may split the Cookie header into one field per
// cookie (RFC 9113 §8.2.3), and the server must join the fields with "; "
// before it hands them to anything that reads one HTTP/1.1-style field.
// net/http's HTTP/2 server does it with strings.Join(cookies, "; "); the
// native engines did not, and Context.Cookie saw the first field only.

// reqHeaders944 is a GET's pseudo-header fields plus extra, in order.
func reqHeaders944(extra ...[2]string) [][2]string {
	return append([][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "example.com"},
	}, extra...)
}

// seen944 is what a handler saw of a request's header fields.
type seen944 struct {
	fields [][2]string
	cookie []string // the values of the cookie fields, in order
}

func (s *seen944) record(st *Stream) {
	s.fields = append([][2]string(nil), st.GetHeaders()...)
	s.cookie = nil
	for _, h := range s.fields {
		if h[0] == "cookie" {
			s.cookie = append(s.cookie, h[1])
		}
	}
}

// send944 delivers a request with the given header fields to a fresh
// Processor in the way named by path, and returns what its handler saw.
// path: "raw" (ProcessRawHeaders, the engines' fast path), "frame" (a
// HEADERS frame through ProcessFrame), "continuation" (HEADERS then
// CONTINUATION).
func send944(t *testing.T, path string, fields [][2]string) *seen944 {
	t.Helper()
	got := &seen944{}
	done := make(chan struct{})
	p := NewProcessor(HandlerFunc(func(_ context.Context, s *Stream) error {
		got.record(s)
		close(done)
		return nil
	}), newTestFrameWriter(), newTestResponseWriter())
	block := encodeHeaders(t, fields)
	switch path {
	case "raw":
		if err := p.ProcessRawHeaders(1, true, block); err != nil {
			t.Fatalf("ProcessRawHeaders: %v", err)
		}
	case "frame":
		if err := p.ProcessFrame(context.Background(), makeHeadersFrame(t, 1, true, true, block)); err != nil {
			t.Fatalf("ProcessFrame HEADERS: %v", err)
		}
	case "continuation":
		var buf bytes.Buffer
		w := http2.NewFramer(&buf, nil)
		mid := len(block) / 2
		if err := w.WriteRawFrame(http2.FrameHeaders, http2.FlagHeadersEndStream, 1, block[:mid]); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteRawFrame(http2.FrameContinuation, http2.FlagContinuationEndHeaders, 1, block[mid:]); err != nil {
			t.Fatal(err)
		}
		r := http2.NewFramer(nil, &buf)
		for range 2 {
			f, err := r.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			if err := p.ProcessFrame(context.Background(), f); err != nil {
				t.Fatalf("ProcessFrame %v: %v", f.Header().Type, err)
			}
		}
	default:
		t.Fatalf("unknown path %q", path)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never ran")
	}
	return got
}

// TestSplitCookieFieldsAreJoined944 sends cookie fields split every way the
// RFC allows and checks that the handler sees exactly one cookie field, whose
// bytes are what net/http's HTTP/2 server builds for the same request
// (strings.Join(fields, "; ")), at the place of the first one.
func TestSplitCookieFieldsAreJoined944(t *testing.T) {
	cookie := func(v string) [2]string { return [2]string{"cookie", v} }
	for _, tc := range []struct {
		name   string
		fields [][2]string
		want   []string // the cookie values the request carries, as the client sent them
	}{
		{"one field is left alone", reqHeaders944(cookie("theme=dark; sid=alice")), []string{"theme=dark; sid=alice"}},
		{"two fields", reqHeaders944(cookie("theme=dark"), cookie("sid=alice")), []string{"theme=dark", "sid=alice"}},
		{"three fields", reqHeaders944(cookie("a=1"), cookie("b=2"), cookie("c=3")), []string{"a=1", "b=2", "c=3"}},
		{"an empty field", reqHeaders944(cookie("a=1"), cookie(""), cookie("c=3")), []string{"a=1", "", "c=3"}},
		{"an empty first field", reqHeaders944(cookie(""), cookie("b=2")), []string{"", "b=2"}},
		{"only empty fields", reqHeaders944(cookie(""), cookie("")), []string{"", ""}},
		{"another field between", reqHeaders944(cookie("a=1"), [2]string{"x-other", "y"}, cookie("b=2"), [2]string{"accept", "*/*"}, cookie("c=3")),
			[]string{"a=1", "b=2", "c=3"}},
		{"each field holding several cookies", reqHeaders944(cookie("a=1; b=2"), cookie("c=3; d=4")), []string{"a=1; b=2", "c=3; d=4"}},
	} {
		for _, path := range []string{"raw", "frame", "continuation"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				got := send944(t, path, tc.fields)
				want := strings.Join(tc.want, "; ") // net/http's h2 server: strings.Join(cookies, "; ")
				if len(got.cookie) != 1 || got.cookie[0] != want {
					t.Fatalf("the handler saw cookie fields %q, want exactly one: %q", got.cookie, want)
				}
				// The joined field sits where the first cookie field did,
				// and nothing else moved or was lost.
				var wantFields [][2]string
				placed := false
				for _, h := range tc.fields {
					if h[0] != "cookie" {
						wantFields = append(wantFields, h)
					} else if !placed {
						wantFields = append(wantFields, [2]string{"cookie", want})
						placed = true
					}
				}
				if len(got.fields) != len(wantFields) {
					t.Fatalf("the handler saw %d fields, want %d:\n got %q\nwant %q", len(got.fields), len(wantFields), got.fields, wantFields)
				}
				for i := range wantFields {
					if got.fields[i] != wantFields[i] {
						t.Fatalf("field %d: got %q, want %q", i, got.fields[i], wantFields[i])
					}
				}
			})
		}
	}
}

// TestCookieJoinStateDoesNotLeakBetweenBlocks944 decodes a split cookie and
// then a request with no cookie, and one with a single cookie, on the same
// Processor: nothing of the first block's join may show in the next.
func TestCookieJoinStateDoesNotLeakBetweenBlocks944(t *testing.T) {
	var seen []*seen944
	p := NewProcessor(HandlerFunc(func(_ context.Context, s *Stream) error {
		x := &seen944{}
		x.record(s)
		seen = append(seen, x)
		return nil
	}), newTestFrameWriter(), newTestResponseWriter())
	for i, fields := range [][][2]string{
		reqHeaders944([2]string{"cookie", "a=1"}, [2]string{"cookie", "b=2"}),
		reqHeaders944(),
		reqHeaders944([2]string{"cookie", "c=3"}),
		reqHeaders944([2]string{"cookie", "d=4"}, [2]string{"cookie", "e=5"}),
	} {
		if err := p.ProcessRawHeaders(uint32(2*i+1), true, encodeHeaders(t, fields)); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	// Handlers are inline here: all four ran.
	want := [][]string{{"a=1; b=2"}, nil, {"c=3"}, {"d=4; e=5"}}
	if len(seen) != len(want) {
		t.Fatalf("%d handlers ran, want %d", len(seen), len(want))
	}
	for i := range want {
		if strings.Join(seen[i].cookie, "|") != strings.Join(want[i], "|") || len(seen[i].cookie) != len(want[i]) {
			t.Errorf("request %d: cookie fields %q, want %q", i, seen[i].cookie, want[i])
		}
	}
}

// TestTrailerCookieFieldsAreNotJoined944 keeps the join to a request's
// headers: a trailer block is left as the peer sent it.
func TestTrailerCookieFieldsAreNotJoined944(t *testing.T) {
	p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
	// A request with a body, so the stream stays open for trailers.
	if err := p.ProcessRawHeaders(1, false, encodeHeaders(t, reqHeaders944([2]string{"cookie", "a=1"}, [2]string{"cookie", "b=2"}))); err != nil {
		t.Fatal(err)
	}
	s, ok := p.manager.GetStream(1)
	if !ok {
		t.Fatal("stream 1 is not open")
	}
	var cookies []string
	for _, h := range s.GetHeaders() {
		if h[0] == "cookie" {
			cookies = append(cookies, h[1])
		}
	}
	if len(cookies) != 1 || cookies[0] != "a=1; b=2" {
		t.Fatalf("request cookie fields %q, want [a=1; b=2]", cookies)
	}
	// joinCookies is off for a trailer block (the flag hpackEmit reads).
	var tr [][2]string
	p.beginHeaderDecode(&tr, false)
	if p.joinCookies {
		t.Fatal("a trailer block joins cookie fields")
	}
	p.ensureHPACKDecoder()
	if _, err := p.hpackDecoder.Write(encodeHeaders(t, [][2]string{{"cookie", "x=1"}, {"cookie", "y=2"}})); err != nil {
		t.Fatal(err)
	}
	_ = p.hpackDecoder.Close()
	p.endHeaderDecode()
	if len(tr) != 2 || tr[0][1] != "x=1" || tr[1][1] != "y=2" {
		t.Fatalf("a trailer block's cookie fields were changed: %q", tr)
	}
}
