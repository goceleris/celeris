//go:build linux

package celeris_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// celeris#885 item 44: a single large WriteResponse over h2c, read by a raw
// http2.Framer client that opens its flow-control windows wide, was reported
// to deliver 90 bytes and then nothing with WriteTimeout 1 s (the original
// probe was not kept, and the bug did not reproduce on main). This is that
// probe as a regression test: one 3.5 MiB response written in one call (c.Blob
// reaches the response writer's WriteResponse), the stream window and the
// connection window both opened to the 2^31-1 maximum so that nothing but the
// server limits the transfer, WriteTimeout 1 s. The client reads frames
// until END_STREAM on the stream and checks the response head, the declared
// content-length, the DATA payload byte for byte, and that no RST_STREAM or
// GOAWAY came first.
//
// Every engine, with the handler inline and as an async route. The client
// gives up after idleCap761 without a frame, so a response that stops
// mid-body fails in seconds with the byte count it had reached.
func TestH2cSingleLargeWriteResponseRawFramer44(t *testing.T) {
	const size = 3<<20 + 512<<10 // 3.5 MiB
	body := bodies761(size)[size]
	const maxWindow = 1<<31 - 1

	for _, e := range engines761 {
		for _, route := range []string{"sync", "async-route"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				addr := startServerConfig761(t, celeris.Config{Engine: e.eng, WriteTimeout: time.Second}, func(s *celeris.Server) {
					r := s.GET("/big44", func(c *celeris.Context) error {
						return c.Blob(http.StatusOK, "application/octet-stream", body)
					})
					if route == "async-route" {
						r.Async()
					}
				})
				c := dialH2raw889(t, addr, h2dial889{initialWindow: maxWindow})
				// The connection window starts at 65535 whatever SETTINGS say.
				if err := c.fr.WriteWindowUpdate(0, maxWindow-65535); err != nil {
					t.Fatal(err)
				}
				c.request(1, "GET", "/big44", true, true)

				var (
					status, clen string
					got          bytes.Buffer
					ended        bool
				)
				dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
					switch f.Name {
					case ":status":
						status = f.Value
					case "content-length":
						clen = f.Value
					}
				})
				for !ended {
					_ = c.conn.SetReadDeadline(time.Now().Add(idleCap761))
					f, err := c.fr.ReadFrame()
					if err != nil {
						t.Fatalf("after %d of %d body bytes (status %q): %s", got.Len(), size, status, describeReadEnd761(err))
					}
					switch f := f.(type) {
					case *http2.SettingsFrame:
						if !f.IsAck() {
							if err := c.fr.WriteSettingsAck(); err != nil {
								t.Fatal(err)
							}
						}
					case *http2.HeadersFrame:
						if f.StreamID == 1 {
							if _, err := dec.Write(f.HeaderBlockFragment()); err != nil {
								t.Fatal(err)
							}
							ended = ended || f.StreamEnded()
						}
					case *http2.DataFrame:
						if f.StreamID == 1 {
							got.Write(f.Data())
							ended = ended || f.StreamEnded()
						}
					case *http2.RSTStreamFrame:
						t.Fatalf("stream reset (%s) after %d of %d body bytes", f.ErrCode, got.Len(), size)
					case *http2.GoAwayFrame:
						t.Fatalf("GOAWAY (%s) after %d of %d body bytes", f.ErrCode, got.Len(), size)
					}
				}
				if status != "200" {
					t.Errorf(":status %q, want 200", status)
				}
				if want := "3670016"; clen != want {
					t.Errorf("content-length %q, want %s", clen, want)
				}
				if got.Len() != size || !bytes.Equal(got.Bytes(), body) {
					t.Errorf("received %d of %d body bytes, equal=%v", got.Len(), size, bytes.Equal(got.Bytes(), body))
				}
			})
		}
	}
}
