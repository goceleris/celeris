package static

import (
	"bytes"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// celeris#888 item 91. A pre-compressed variant that is a symlink out of the
// Root made the identity file a 400 for a client that accepts the coding, and
// a symlink out of the Root answered 400 where a missing name is passed on (a
// 404): the status told a client which names exist and which are links.

// TestEscapingVariantServesIdentity888: a variant that leaves the Root is no
// variant, as one that leads to a hidden file or is not a regular file never
// was: the identity file is served, with its own validators and no
// Content-Encoding, and a variant that stays inside is still preferred.
func TestEscapingVariantServesIdentity888(t *testing.T) {
	secretTag := weakETag(secretTime888, int64(len(secretBytes888)))
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, r root888)
		ae       string
		wantEnc  string
		wantBody string
	}{
		{"gz-escapes", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), "app.js.gz")
		}, "gzip", "", "plain"},
		{"gz-escapes-relative", func(t *testing.T, r root888) {
			r.link(t, filepath.Join("..", "outside", "secret.txt"), "app.js.gz")
		}, "gzip, br", "", "plain"},
		{"br-escapes-gz-inside", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), "app.js.br")
			r.write(t, filepath.Join(r.root, "app.js.gz"), []byte("gz-bytes"), secretTime888)
		}, "gzip, br", "gzip", "gz-bytes"},
		{"both-escape", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), "app.js.br")
			r.link(t, filepath.Join(r.outside, "secret.txt"), "app.js.gz")
		}, "gzip, br", "", "plain"},
		{"control-no-variant", func(t *testing.T, r root888) {}, "gzip", "", "plain"},
		{"control-inside", func(t *testing.T, r root888) {
			r.write(t, filepath.Join(r.root, "app.js.gz"), []byte("gz-bytes"), secretTime888)
		}, "gzip", "gzip", "gz-bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRoot888(t)
			r.write(t, filepath.Join(r.root, "app.js"), []byte("plain"), secretTime888.Add(-time.Hour))
			tc.setup(t, r)
			mw := New(Config{Root: r.root, Compress: true})
			rec, err := get888(t, mw, "/app.js", "accept-encoding", tc.ae)
			if err != nil {
				t.Fatalf("GET /app.js: %v, want %d (the variant that leaves the Root is no variant)", err, 200)
			}
			if rec.StatusCode != 200 || rec.BodyString() != tc.wantBody || rec.Header("content-encoding") != tc.wantEnc {
				t.Errorf("GET /app.js: %d %q content-encoding %q; want 200 %q %q",
					rec.StatusCode, rec.Body, rec.Header("content-encoding"), tc.wantBody, tc.wantEnc)
			}
			if tc.wantEnc == "" && rec.Header("etag") == secretTag {
				t.Errorf("the response carries the ETag %s of the file outside the Root", secretTag)
			}
		})
	}
}

// TestEscapingSymlinkIsAnsweredAsMissing888: a symlink out of the Root, a path
// below a symlinked directory out of it, and the directory index that leaves it,
// are answered exactly as a name that is not there: passed on to the next
// handler (the 404), or the index in SPA mode. The status of the two does not
// tell which exists.
func TestEscapingSymlinkIsAnsweredAsMissing888(t *testing.T) {
	r := newRoot888(t)
	r.link(t, filepath.Join(r.outside, "secret.txt"), "leak.txt")
	r.link(t, r.outside, "linkdir")
	r.link(t, filepath.Join(r.outside, "secret.txt"), filepath.Join("sub", "index.html"))
	r.write(t, filepath.Join(r.root, "index.html"), []byte("spa-index"), secretTime888)
	for _, cfg := range []Config{{Root: r.root}, {Root: r.root, Compress: true}, {Root: r.root, SPA: true}} {
		mw := New(cfg)
		for _, p := range []string{"/leak.txt", "/linkdir/secret.txt", "/sub/"} {
			for _, hdr := range [][]string{nil, {"if-none-match", `"x"`}, {"range", "bytes=0-3"}} {
				gotRec, gotErr := get888(t, mw, p, hdr...)
				missing := "/nothing-here.txt"
				if p == "/sub/" {
					missing = "/sub2/"
				}
				wantRec, wantErr := get888(t, mw, missing, hdr...)
				if (gotErr == nil) != (wantErr == nil) || gotRec.StatusCode != wantRec.StatusCode || gotRec.BodyString() != wantRec.BodyString() {
					t.Errorf("SPA=%v Compress=%v GET %s %v: status %d err %v body %q; a missing name gives status %d err %v body %q",
						cfg.SPA, cfg.Compress, p, hdr, gotRec.StatusCode, gotErr, gotRec.Body, wantRec.StatusCode, wantErr, wantRec.Body)
				}
				if bytes.Contains(gotRec.Body, secretBytes888) {
					t.Errorf("GET %s %v: the file outside the Root was served", p, hdr)
				}
			}
		}
	}
}

// TestEscapingVariantConditionalLeaksNothing888: the conditional rows the
// "gzip-variant" case of TestRoot304DoesNotFollowSymlinkOut888 held. A client
// that guesses the outside file's tag or date gets nothing of it: no 304 for
// the guess and none of its validators. (A 304 for the identity file, whose
// own mtime the If-Modified-Since may pass, is legitimate.)
func TestEscapingVariantConditionalLeaksNothing888(t *testing.T) {
	secretTag := weakETag(secretTime888, int64(len(secretBytes888)))
	secretDate := secretTime888.UTC().Format(http.TimeFormat)
	identityTime := secretTime888.Add(-time.Hour)
	for _, hdr := range [][]string{
		{"accept-encoding", "gzip", "if-none-match", secretTag},
		{"accept-encoding", "gzip", "if-modified-since", secretDate},
		{"accept-encoding", "gzip", "if-none-match", `"x"`},
	} {
		r := newRoot888(t)
		r.write(t, filepath.Join(r.root, "app.js"), []byte("plain"), identityTime)
		r.link(t, filepath.Join(r.outside, "secret.txt"), "app.js.gz")
		rec, err := get888(t, New(Config{Root: r.root, Compress: true}), "/app.js", hdr...)
		if err != nil {
			t.Fatalf("GET /app.js %v: %v", hdr, err)
		}
		if rec.StatusCode == 304 && rec.Header("etag") == secretTag {
			t.Errorf("GET /app.js %v: a 304 for the outside file's tag", hdr)
		}
		if rec.Header("etag") == secretTag || rec.Header("last-modified") == secretDate || rec.Header("content-encoding") != "" {
			t.Errorf("GET /app.js %v: etag %q last-modified %q content-encoding %q describe the outside file",
				hdr, rec.Header("etag"), rec.Header("last-modified"), rec.Header("content-encoding"))
		}
		if bytes.Contains(rec.Body, secretBytes888) {
			t.Errorf("GET /app.js %v: the outside file was served", hdr)
		}
	}
}
