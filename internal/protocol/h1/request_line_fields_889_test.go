package h1

import (
	"errors"
	"testing"
)

// celeris#889 (review r1 of the PR): the other two fields of the request line.
//
// The version was matched on 4 of its 8 bytes, so "HTTP\n1.1", "H\x00TP/1.1" and
// "HXXXX1.1" were HTTP/1.1; a front end that takes a bare LF as the end of a
// line reads "GET / HTTP" and then a header "1.1". And an empty request-target
// ("GET  HTTP/1.1", two spaces) was accepted with Path "".
func TestParseRequest_VersionIsAllEightBytes889(t *testing.T) {
	for _, v := range []string{"HTTP\n1.1", "H\x00TP/1.1", "HXXXX1.1", "HTTP/1\x001", "HTTP/1.2", "HTTP/2.0", "http/1.1", "HTTP/1.", "HTTP/1.11", "HTTP\r1.1", "HTTP/1.1 ", " HTTP/1.1"} {
		_, err := parseLine889("GET / " + v)
		if err == nil {
			t.Errorf("version %q accepted", v)
			continue
		}
		if !errors.Is(err, ErrUnsupportedVersion) && !errors.Is(err, ErrInvalidRequestLine) {
			t.Errorf("version %q: err = %v", v, err)
		}
	}
	for _, v := range []string{"HTTP/1.1", "HTTP/1.0"} {
		req, err := parseLine889("GET / " + v)
		if err != nil {
			t.Errorf("version %q: %v", v, err)
		} else if req.Version != v {
			t.Errorf("version %q: Version = %q", v, req.Version)
		}
	}
}

func TestParseRequest_EmptyTargetIsInvalid889(t *testing.T) {
	for _, line := range []string{"GET  HTTP/1.1", "POST  HTTP/1.1", "PROPFIND  HTTP/1.0"} {
		if _, err := parseLine889(line); !errors.Is(err, ErrInvalidRequestLine) {
			t.Errorf("%q: err = %v, want ErrInvalidRequestLine", line, err)
		}
	}
}
