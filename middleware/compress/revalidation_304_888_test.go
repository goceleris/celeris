package compress

import (
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/etag"
	"github.com/goceleris/celeris/middleware/static"
)

// celeris#888 item 36 (celeris#846 review of #972). A 304 must carry the tag
// a 200 to the same request would carry (RFC 9110 15.4.5), or a cache cannot
// freshen the response it holds with it (RFC 9111 4.3.4). compress weakens the
// strong tag of a response it encodes, W/"hash", but a 304 has no body to
// encode, so it passed the strong tag through: the 200 said W/"hash" and the
// revalidation of it said "hash". static (over fs.FS) and etag mark a response
// whose strong tag compress would weaken, and compress applies to the 304 the
// rule it applies to the 200: weak when it negotiates an encoding and the
// representation is one it encodes (MinLength, ExcludedContentTypes).

// TestCompress304TagIsThe200s888 is the probe of the #972 review
// (TestReviewProbeCompress304Tag846) as a regression test.
func TestCompress304TagIsThe200s888(t *testing.T) {
	content := []byte(strings.Repeat("0123456789", 500))
	mod := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	mw := static.New(static.Config{FS: fstest.MapFS{"f.txt": {Data: content, ModTime: mod}}})
	run := func(opts ...celeristest.Option) *celeristest.ResponseRecorder {
		t.Helper()
		all := append([]celeristest.Option{celeristest.WithHandlers(New(), mw)}, opts...)
		c, rec := celeristest.NewContextT(t, "GET", "/f.txt", all...)
		if err := c.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
		return rec
	}
	gz := run(celeristest.WithHeader("accept-encoding", "gzip"))
	if gz.Header("content-encoding") != "gzip" || !strings.HasPrefix(gz.Header("etag"), `W/"`) {
		t.Fatalf("200: content-encoding %q etag %q, want gzip with a weak tag", gz.Header("content-encoding"), gz.Header("etag"))
	}
	nm := run(celeristest.WithHeader("accept-encoding", "gzip"), celeristest.WithHeader("if-none-match", gz.Header("etag")))
	if nm.StatusCode != 304 || nm.Header("etag") != gz.Header("etag") {
		t.Errorf("revalidation: %d etag %q, want 304 with the 200's %q", nm.StatusCode, nm.Header("etag"), gz.Header("etag"))
	}
}

// rep888 is one file the fixtures serve.
type rep888 struct {
	name string
	data []byte
}

func fixtures888() []rep888 {
	text := []byte(strings.Repeat("0123456789", 500)) // 5000 bytes, compresses well
	noise := make([]byte, 2000)
	rand.New(rand.NewSource(1)).Read(noise) // does not compress: every encoding expands it
	return []rep888{
		{"big.txt", text},
		{"small.txt", []byte(strings.Repeat("x", 200))}, // under the default MinLength (256)
		{"pic.png", text},    // image/png: excluded by default
		{"noise.bin", noise}, // not excluded, but the encoded form is longer
		{"empty.txt", nil},
	}
}

// staticFS888 serves fixtures888 from an fs.FS (strong tag, a hash of the bytes).
func staticFS888(t *testing.T) celeris.HandlerFunc {
	t.Helper()
	mod := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	m := fstest.MapFS{}
	for _, f := range fixtures888() {
		m[f.name] = &fstest.MapFile{Data: f.data, ModTime: mod}
	}
	return static.New(static.Config{FS: m})
}

// staticRoot888 serves fixtures888 from a directory (weak tag, mtime and size).
func staticRoot888(t *testing.T) celeris.HandlerFunc {
	t.Helper()
	dir := t.TempDir()
	mod := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	for _, f := range fixtures888() {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	return static.New(static.Config{Root: dir})
}

// handler888 answers every path with the fixture of that name; it sets no
// validator, so the etag middleware in front of it makes the tag.
func handler888(c *celeris.Context) error {
	for _, f := range fixtures888() {
		if "/"+f.name == c.Path() {
			ct := "text/plain"
			if strings.HasSuffix(f.name, ".png") {
				ct = "image/png"
			} else if strings.HasSuffix(f.name, ".bin") {
				ct = "application/octet-stream"
			}
			return c.Blob(200, ct, f.data)
		}
	}
	return c.Next()
}

// ownTag888 is a handler that sets its own strong tag, as a handler does for
// a document it versions (the etag middleware reuses it).
func ownTag888(c *celeris.Context) error {
	c.SetHeader("etag", `"v7"`)
	return handler888(c)
}

// newCompress888 is New over the defaults, with the fields cfg sets (a zero
// Config means the defaults, not MinLength 0).
func newCompress888(cfg Config) celeris.HandlerFunc {
	c := defaultConfig
	if cfg.MinLength != 0 {
		c.MinLength = cfg.MinLength
	}
	if cfg.Encodings != nil {
		c.Encodings = cfg.Encodings
	}
	if cfg.ExcludedContentTypes != nil {
		c.ExcludedContentTypes = cfg.ExcludedContentTypes
	}
	return New(c)
}

type chain888 struct {
	name  string
	build func(t *testing.T, cfg Config) []celeris.HandlerFunc
	// weakIdentity: the tag of a response that is not encoded is weak anyway
	// (Root's mtime and size, the etag middleware's default).
	weakIdentity bool
	// outerEtag: the etag middleware wraps compress, so it tags the bytes
	// that go out (encoded or not) and compress never sees a tag.
	outerEtag bool
	// static: the chain ends in middleware/static, which answers Range,
	// If-Range and If-Modified-Since; the handler chains answer none of them.
	static bool
}

func chains888() []chain888 {
	return []chain888{
		{name: "static-FS", static: true, build: func(t *testing.T, cfg Config) []celeris.HandlerFunc {
			return []celeris.HandlerFunc{newCompress888(cfg), staticFS888(t)}
		}},
		{name: "static-Root", static: true, weakIdentity: true, build: func(t *testing.T, cfg Config) []celeris.HandlerFunc {
			return []celeris.HandlerFunc{newCompress888(cfg), staticRoot888(t)}
		}},
		{name: "compress-etag-strong-handler", build: func(t *testing.T, cfg Config) []celeris.HandlerFunc {
			return []celeris.HandlerFunc{newCompress888(cfg), etag.New(etag.Config{Strong: true}), handler888}
		}},
		{name: "etag-strong-compress-handler", outerEtag: true, build: func(t *testing.T, cfg Config) []celeris.HandlerFunc {
			return []celeris.HandlerFunc{etag.New(etag.Config{Strong: true}), newCompress888(cfg), handler888}
		}},
		{name: "compress-etag-weak-handler", weakIdentity: true, build: func(t *testing.T, cfg Config) []celeris.HandlerFunc {
			return []celeris.HandlerFunc{newCompress888(cfg), etag.New(), handler888}
		}},
		{name: "compress-etag-handlers-own-tag", build: func(t *testing.T, cfg Config) []celeris.HandlerFunc {
			return []celeris.HandlerFunc{newCompress888(cfg), etag.New(), ownTag888}
		}},
		{name: "compress-etag-static-FS", static: true, build: func(t *testing.T, cfg Config) []celeris.HandlerFunc {
			return []celeris.HandlerFunc{newCompress888(cfg), etag.New(etag.Config{Strong: true}), staticFS888(t)}
		}},
		{name: "etag-compress-static-FS", static: true, build: func(t *testing.T, cfg Config) []celeris.HandlerFunc {
			return []celeris.HandlerFunc{etag.New(etag.Config{Strong: true}), newCompress888(cfg), staticFS888(t)}
		}},
	}
}

// TestCompress304CarriesTheTagOfThe200888: for every chain, file, compress
// configuration and request shape, the 304 answering a revalidation carries
// exactly the ETag the same request's 200 (or 206 or 416) carries, for
// If-None-Match holding that tag, its other-strength form, a list that holds
// it, and "*" (and for If-Modified-Since where the chain answers it). The tag
// of the 200 itself is pinned too, so that "equal" is not reached by weakening
// nothing, or everything: a response compress encodes has a weak tag, and so
// has one it sends as it is because the encoded form is longer (it would not
// know that of the 304); a response it does not encode keeps the tag its
// handler made.
func TestCompress304CarriesTheTagOfThe200888(t *testing.T) {
	type req struct {
		method  string
		headers [][2]string
		// acceptsGzip: compress negotiates gzip for this request.
		acceptsGzip bool
		// static chains: the status the handler answers (200 where the
		// request's Range is not satisfied by a part).
		staticStatus int
	}
	gzip := [2]string{"accept-encoding", "gzip"}
	reqs := map[string]req{
		"GET-gzip":               {"GET", [][2]string{gzip}, true, 200},
		"GET-no-AE":              {"GET", nil, false, 200},
		"GET-identity":           {"GET", [][2]string{{"accept-encoding", "identity"}}, false, 200},
		"GET-refuses-gzip":       {"GET", [][2]string{{"accept-encoding", "gzip;q=0"}}, false, 200},
		"HEAD-gzip":              {"HEAD", [][2]string{gzip}, false, 200},
		"GET-range-gzip":         {"GET", [][2]string{gzip, {"range", "bytes=0-9"}}, true, 206},
		"GET-bad-range-gzip":     {"GET", [][2]string{gzip, {"range", "bytes=99999-"}}, true, 416},
		"GET-range-ifrange-miss": {"GET", [][2]string{gzip, {"range", "bytes=0-9"}, {"if-range", `"nope"`}}, true, 200},
	}
	type row struct {
		file string
		cfg  Config
		req  string
		// enc: compress encodes this file under cfg when it negotiates an
		// encoding. expand: it would encode it but the result is longer.
		enc, expand bool
	}
	rows := []row{
		{file: "big.txt", req: "GET-gzip", enc: true},
		{file: "big.txt", req: "GET-no-AE"},
		{file: "big.txt", req: "GET-identity"},
		{file: "big.txt", req: "GET-refuses-gzip"},
		{file: "big.txt", req: "HEAD-gzip", enc: true},
		{file: "big.txt", req: "GET-range-gzip", enc: true},
		{file: "big.txt", req: "GET-bad-range-gzip", enc: true},
		{file: "big.txt", req: "GET-range-ifrange-miss", enc: true},
		{file: "big.txt", cfg: Config{MinLength: 5000}, req: "GET-gzip", enc: true}, // the size: encoded
		{file: "big.txt", cfg: Config{MinLength: 5001}, req: "GET-gzip"},            // one under it: not
		{file: "big.txt", cfg: Config{ExcludedContentTypes: []string{"text/"}}, req: "GET-gzip"},
		{file: "big.txt", cfg: Config{ExcludedContentTypes: []string{}}, req: "GET-gzip", enc: true},
		{file: "big.txt", cfg: Config{Encodings: []string{"zstd"}}, req: "GET-gzip"}, // none in common
		{file: "small.txt", req: "GET-gzip"},                                         // under the default MinLength
		{file: "small.txt", cfg: Config{MinLength: 1}, req: "GET-gzip", enc: true},
		{file: "pic.png", req: "GET-gzip"}, // image/png: excluded by default
		{file: "noise.bin", req: "GET-gzip", expand: true},
		{file: "empty.txt", req: "GET-gzip"},
	}

	for _, ch := range chains888() {
		for _, r := range rows {
			if r.cfg.Encodings != nil && ch.outerEtag {
				continue
			}
			if r.file == "empty.txt" && !ch.static {
				continue // the etag middleware passes an empty body through, whatever the request holds
			}
			name := ch.name + "/" + r.file + "/" + r.req
			if r.cfg.MinLength != 0 {
				name += "/MinLength" + strconv.Itoa(r.cfg.MinLength)
			}
			if r.cfg.ExcludedContentTypes != nil {
				name += "/Excluded" + strings.Join(r.cfg.ExcludedContentTypes, "+")
			}
			if r.cfg.Encodings != nil {
				name += "/Encodings" + strings.Join(r.cfg.Encodings, "+")
			}
			t.Run(name, func(t *testing.T) {
				rq := reqs[r.req]
				hs := ch.build(t, r.cfg)
				do := func(extra ...[2]string) *celeristest.ResponseRecorder {
					t.Helper()
					opts := []celeristest.Option{celeristest.WithHandlers(hs...)}
					for _, h := range append(append([][2]string{}, rq.headers...), extra...) {
						opts = append(opts, celeristest.WithHeader(h[0], h[1]))
					}
					c, rec := celeristest.NewContextT(t, rq.method, "/"+r.file, opts...)
					if err := c.Next(); err != nil {
						t.Fatalf("Next: %v", err)
					}
					return rec
				}
				ref := do()
				wantStatus := 200
				if ch.static {
					wantStatus = rq.staticStatus
				}
				if ref.StatusCode != wantStatus {
					t.Fatalf("the request answers %d, want %d", ref.StatusCode, wantStatus)
				}
				tag := ref.Header("etag")
				if tag == "" {
					t.Fatalf("the response (status %d) carries no ETag", ref.StatusCode)
				}
				// compress encodes a full GET 200 it negotiates an encoding for.
				encoded := !ch.outerEtag && r.enc && rq.acceptsGzip && rq.method == "GET" && ref.StatusCode == 200
				if got := ref.Header("content-encoding"); (got != "") != encoded && !ch.outerEtag {
					t.Fatalf("content-encoding %q on the 200, want encoded=%v", got, encoded)
				}
				// Weak: Root's and etag's default tags; the tag of an encoded
				// response; the tag of one compress would encode but for the
				// size of the result. Strong otherwise.
				wantWeak := ch.weakIdentity || encoded ||
					(!ch.outerEtag && r.expand && rq.acceptsGzip && rq.method == "GET" && ref.StatusCode == 200)
				if isWeak := strings.HasPrefix(tag, "W/"); isWeak != wantWeak {
					t.Fatalf("the response (status %d, content-encoding %q) carries ETag %q, want weak=%v", ref.StatusCode, ref.Header("content-encoding"), tag, wantWeak)
				}
				opaque := strings.TrimPrefix(tag, "W/")
				conds := [][2]string{}
				for _, inm := range []string{tag, opaque, "W/" + opaque, `"nope", ` + tag, `W/"nope", ` + opaque, "*"} {
					conds = append(conds, [2]string{"if-none-match", inm})
				}
				if ch.static {
					conds = append(conds, [2]string{"if-modified-since", "Fri, 03 Jul 2026 10:00:00 GMT"})
				}
				for _, cond := range conds {
					nm := do(cond)
					if nm.StatusCode != 304 {
						t.Errorf("%s: %q: status %d, want 304", cond[0], cond[1], nm.StatusCode)
						continue
					}
					if got := nm.Header("etag"); got != tag {
						t.Errorf("%s: %q: the 304 carries ETag %q, the response it revalidates carries %q (RFC 9110 15.4.5)", cond[0], cond[1], got, tag)
					}
				}
			})
		}
	}
}
