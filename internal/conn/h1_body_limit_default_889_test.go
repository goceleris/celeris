package conn

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// Polish item 14 of celeris#889: the default MaxRequestBodySize is 4 MiB.
// This is the ProcessH1 level of the table in test/server/bodylimit_default_889_linux_test.go:
// the limit the engines hand to H1State is what resource.Config.WithDefaults
// resolves for 0, an explicit value and -1, and a body of exactly that size is
// served while one byte more is answered 413, for a Content-Length body and a
// chunked one, each fed in a single call. The chunked body over several calls
// is h1_chunked_split_1029_test.go.

type bodyLenHandler889 struct{ got []int }

func (h *bodyLenHandler889) HandleStream(_ context.Context, s *stream.Stream) error {
	h.got = append(h.got, len(s.GetData()))
	return nil
}

func TestProcessH1BodyLimitFromResolvedConfig889(t *testing.T) {
	const miB = 1 << 20
	cases := []struct {
		name    string
		cfg     int64 // Config.MaxRequestBodySize
		max     int64 // the effective limit, -1 unlimited
		wantCfg int64 // what WithDefaults hands the engines (0 = unlimited)
	}{
		{"default", 0, 4 * miB, 4 * miB},
		{"explicit-8MiB", 8 * miB, 8 * miB, 8 * miB},
		{"unlimited", -1, -1, 0},
		{"explicit-4MiB", 4 * miB, 4 * miB, 4 * miB},
	}
	for _, tc := range cases {
		resolved := resource.Config{MaxRequestBodySize: tc.cfg}.WithDefaults().MaxRequestBodySize
		if resolved != tc.wantCfg {
			t.Errorf("%s: WithDefaults resolved MaxRequestBodySize %d, want %d", tc.name, resolved, tc.wantCfg)
		}
		for _, n := range []int{4 * miB, 4*miB + 1} {
			for _, chunked := range []bool{false, true} {
				name := fmt.Sprintf("%s/%d/cl", tc.name, n)
				if chunked {
					name = fmt.Sprintf("%s/%d/chunked", tc.name, n)
				}
				t.Run(name, func(t *testing.T) {
					st := NewH1State()
					st.MaxRequestBodySize = resolved
					h := &bodyLenHandler889{}
					var out bytes.Buffer
					var req bytes.Buffer
					body := bytes.Repeat([]byte{'x'}, n)
					if chunked {
						req.WriteString("POST /up HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n")
						for off := 0; off < n; off += miB {
							end := min(off+miB, n)
							fmt.Fprintf(&req, "%x\r\n", end-off)
							req.Write(body[off:end])
							req.WriteString("\r\n")
						}
						req.WriteString("0\r\n\r\n")
					} else {
						fmt.Fprintf(&req, "POST /up HTTP/1.1\r\nHost: a\r\nContent-Length: %d\r\n\r\n", n)
						req.Write(body)
					}
					err := ProcessH1(context.Background(), req.Bytes(), st, h, func(b []byte) { out.Write(b) })
					ok := tc.max < 0 || int64(n) <= tc.max
					if ok {
						if err != nil || len(h.got) != 1 || h.got[0] != n {
							t.Fatalf("limit %s, %d body bytes: err %v, handler saw %v, want one call with %d bytes", tc.name, n, err, h.got, n)
						}
						return
					}
					if err == nil || len(h.got) != 0 || !strings.HasPrefix(out.String(), "HTTP/1.1 413") {
						t.Fatalf("limit %s, %d body bytes: err %v, handler saw %v, answer %q, want an error, no handler call and 413", tc.name, n, err, h.got, firstLine889(out.String()))
					}
				})
			}
		}
	}
}

func firstLine889(s string) string {
	line, _, _ := strings.Cut(s, "\r\n")
	return line
}
