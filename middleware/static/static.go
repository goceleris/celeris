package static

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/httprange"
)

// cachedFile stores the immutable content and content-type of an fs.FS file,
// plus the pre-formatted last-modified and ETag strings. fs.FS is assumed
// immutable (embed.FS, fstest.MapFS), so the formatted header strings
// never go stale once populated. Freshness is verified against modTime
// on every serve — a modTime mismatch invalidates the cached headers.
type cachedFile struct {
	data            []byte
	contentType     string
	etag            string
	lastModifiedStr string
	modTime         time.Time
}

// addVary adds Vary: Accept-Encoding to c's response unless a Vary line
// already names it (or is "*"): a middleware before static that negotiates
// with Context.AcceptsEncodings, such as compress, has added one since
// celeris#912. The response then names it once.
func addVary(c *celeris.Context) {
	for _, h := range c.ResponseHeaders() {
		if h[0] != "vary" {
			continue
		}
		for v := h[1]; v != ""; {
			tok := v
			if i := strings.IndexByte(v, ','); i >= 0 {
				tok, v = v[:i], v[i+1:]
			} else {
				v = ""
			}
			tok = strings.TrimSpace(tok)
			if tok == "*" || strings.EqualFold(tok, "Accept-Encoding") {
				return
			}
		}
	}
	c.AddHeader("vary", "Accept-Encoding")
}

// maxFSFileSize caps in-memory reads from fs.FS to 100 MB.
const maxFSFileSize = 100 << 20

// New creates a static file middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	prefix := cfg.Prefix
	index := cfg.Index
	browse := cfg.Browse
	spa := cfg.SPA
	maxAge := cfg.MaxAge
	root := cfg.Root
	fsys := cfg.FS
	compress := cfg.Compress

	// Precompute cleanRoot once instead of per request.
	var cleanRoot string
	if root != "" {
		cleanRoot = filepath.Clean(root)
	}

	// Precompute the cache-control header once — maxAge is closure-constant,
	// so rebuilding "public, max-age=N" per request wasted an alloc + an
	// strconv.Itoa on every cache-hit response.
	var cacheControl string
	if maxAge > 0 {
		cacheControl = "public, max-age=" + strconv.Itoa(int(maxAge.Seconds()))
	}

	// Per-file cache for fs.FS content. Since fs.FS is immutable (especially
	// embed.FS), caching avoids repeated heap allocations for the same file.
	var fsCache sync.Map // map[string]*cachedFile

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		m := c.Method()
		if m != "GET" && m != "HEAD" {
			return c.Next()
		}

		reqPath := c.Path()

		if prefix != "/" {
			if !strings.HasPrefix(reqPath, prefix) {
				return c.Next()
			}
			// Ensure prefix matches at a segment boundary.
			if len(reqPath) > len(prefix) && reqPath[len(prefix)] != '/' {
				return c.Next()
			}
			reqPath = reqPath[len(prefix):]
			if reqPath == "" {
				reqPath = "/"
			}
		}

		// Clean the path: strip leading slash and trailing slash for fs.FS
		// compatibility (fs.FS paths must not have leading/trailing slashes).
		filePath := strings.TrimPrefix(reqPath, "/")
		filePath = strings.TrimSuffix(filePath, "/")

		if fsys != nil {
			return serveFS(c, fsys, filePath, index, browse, spa, compress, cacheControl, &fsCache)
		}
		return serveOS(c, cleanRoot, filePath, index, browse, spa, cacheControl, compress)
	}
}

// serveOS serves a file from the OS filesystem using c.FileFromDir (which
// handles directory traversal, symlink escape, and range requests).
// cleanRoot must be filepath.Clean(root) — precomputed by New.
func serveOS(c *celeris.Context, cleanRoot, filePath, index string, browse, spa bool, cacheControl string, compress bool) error {
	fullPath := filepath.Join(cleanRoot, filepath.FromSlash(filePath))

	// Prevent directory traversal: filepath.Join resolves ".." segments,
	// so verify the result stays within the root directory.
	if fullPath != cleanRoot && !strings.HasPrefix(fullPath, cleanRoot+string(filepath.Separator)) {
		return c.Next()
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			if spa {
				return c.FileFromDir(cleanRoot, index)
			}
			return c.Next()
		}
		return err
	}

	if info.IsDir() {
		// Try index file in the directory.
		indexPath := filepath.Join(fullPath, index)
		indexInfo, indexErr := os.Stat(indexPath)
		if indexErr == nil && !indexInfo.IsDir() {
			filePath = filepath.Join(filePath, index)
			// fullPath follows the file served: its pre-compressed variant is
			// "<dir>/index.html.gz", never "<dir>.gz" (a sibling) or, for the
			// root itself, "<root>.gz" (a file outside the root).
			fullPath = indexPath
			info = indexInfo
		} else if browse {
			// Resolve symlinks and recheck to prevent symlink escape,
			// mirroring the protection in Context.FileFromDir.
			resolved, err := filepath.EvalSymlinks(fullPath)
			if err != nil {
				return c.Next()
			}
			resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
			if err != nil {
				return c.Next()
			}
			if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
				return c.Next()
			}
			return serveDirListingOS(c, resolved)
		} else {
			return c.Next()
		}
	}

	if serveOSHook != nil {
		serveOSHook()
	}

	// The validators of a response that sends the file come from the file as
	// it is opened for serving (serveOSFile), and from the pre-compressed
	// variant when one is served; this stat only decided that there is a file,
	// and answers a 304.
	if compress {
		if served, err := servePreCompressed(c, cleanRoot, filePath, fullPath, cacheControl); served {
			return err
		}
	}

	return serveOSFile(c, cleanRoot, filePath, "", "", cacheControl, info)
}

// serveOSHook is a test seam (celeris#846 item 4): when set it runs in serveOS
// after the path was stat-ed and before the file is served, which is where
// the path used to be replaced under the validators.
var serveOSHook func()

// serveOSFile serves filePath under cleanRoot through Context.FileFromDir.
// early is a stat of the path made just before.
//
// A conditional request is answered from early when it holds: a 304 sends no
// body, so there is nothing for a replaced file to contradict, and it costs one
// stat, as it always did (FileFromDir resolves symlinks twice and opens the
// file). Every response that sends the file gets its Last-Modified, ETag and
// Cache-Control from the mtime and size of the file as it is opened: early
// could describe another file than the one opened when the path is replaced in
// between (celeris#846). So the Range and If-Range that File then decides are
// about the very bytes sent.
//
// contentType, when not empty, is the response's type (a pre-compressed
// variant is served with its original's, not the one its ".gz" maps to).
// encoding, when not empty, is set as Content-Encoding on the response that
// carries the file, not on a 304; File drops it again from a 416.
func serveOSFile(c *celeris.Context, cleanRoot, filePath, contentType, encoding, cacheControl string, early os.FileInfo) error {
	if c.Header("if-none-match") != "" || c.Header("if-modified-since") != "" {
		// Nothing goes on the response until the 304 is decided: early is a
		// stat, which follows a symlink out of the root before FileFromDir
		// refuses it, and a refused path must not leave its target's tag and
		// date on the error. A request that does not hold gets its validators
		// from the open file below.
		var etag string
		if !early.ModTime().IsZero() {
			etag = weakETag(early.ModTime(), early.Size())
		}
		if notModified(c, etag, early.ModTime()) {
			setCacheHeaders(c, early.ModTime(), early.Size(), cacheControl)
			return c.NoContent(304)
		}
	}
	return ctxkit.FileFromDir(c, cleanRoot, filePath, contentType, func(modTime time.Time, size int64) {
		setCacheHeaders(c, modTime, size, cacheControl)
		if encoding != "" {
			c.SetHeader("content-encoding", encoding)
		}
	})
}

// servePreCompressed checks for .br or .gz pre-compressed variants of the
// requested file and serves them if the client accepts the encoding. The
// variant is the representation the client gets and keeps, so its validators,
// 304, If-Range and Content-Range are its own (celeris#846), and it is served
// with the original's content type. Returns (true, err) if a pre-compressed
// file was served, (false, nil) to fall through to normal serving.
func servePreCompressed(c *celeris.Context, cleanRoot, filePath, fullPath, cacheControl string) (bool, error) {
	ae := c.Header("accept-encoding")
	if ae == "" {
		return false, nil
	}

	// Prefer Brotli over gzip.
	for _, v := range [...]struct{ suffix, encoding string }{{".br", "br"}, {".gz", "gzip"}} {
		if !strings.Contains(ae, v.encoding) {
			continue
		}
		vi, err := os.Stat(fullPath + v.suffix)
		if err != nil {
			continue
		}
		ct := mime.TypeByExtension(filepath.Ext(filePath))
		if ct == "" {
			ct = "application/octet-stream"
		}
		// Vary goes out on the 304 too.
		addVary(c)
		return true, serveOSFile(c, cleanRoot, filePath+v.suffix, ct, v.encoding, cacheControl, vi)
	}

	return false, nil
}

// serveFS serves a file from an fs.FS with ETag/Last-Modified and range support.
func serveFS(c *celeris.Context, fsys fs.FS, filePath, index string, browse, spa, compress bool, cacheControl string, cache *sync.Map) error {
	// 1. Open + stat + directory handling.
	openPath := filePath
	if openPath == "" {
		openPath = "."
	}

	f, err := fsys.Open(openPath)
	if err != nil {
		if spa {
			return serveFS(c, fsys, index, index, browse, false, compress, cacheControl, cache)
		}
		return c.Next()
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return err
	}

	if stat.IsDir() {
		_ = f.Close()
		var indexPath string
		if filePath == "" {
			indexPath = index
		} else {
			indexPath = filePath + "/" + index
		}
		indexFile, indexErr := fsys.Open(indexPath)
		if indexErr == nil {
			indexStat, statErr := indexFile.Stat()
			_ = indexFile.Close()
			if statErr == nil && !indexStat.IsDir() {
				return serveFS(c, fsys, indexPath, index, browse, spa, compress, cacheControl, cache)
			}
		}
		if browse {
			return serveDirListingFS(c, fsys, openPath)
		}
		return c.Next()
	}

	// 2. Size cap check.
	size := stat.Size()
	if size > maxFSFileSize {
		return celeris.NewHTTPError(413, "file exceeds 100MB limit")
	}

	// 3. Pre-compressed check (if enabled). A variant is its own
	// representation: it brings its own validators, 304 and range, so it is
	// tried before the original's validators are set.
	if compress {
		if served, err := servePreCompressedFS(c, fsys, filePath, cacheControl); served {
			return err
		}
	}

	// 4. Content, from the per-file cache when present and the modTime
	// matches, else read and cached. The cached ETag is a hash of the bytes
	// (celeris#846), so a cold request, a 304 included, reads the file first.
	modTime := stat.ModTime()
	var cached *cachedFile
	if cf, ok := cache.Load(filePath); ok {
		if cf2 := cf.(*cachedFile); cf2.modTime.Equal(modTime) {
			cached = cf2
		}
	}
	if cached == nil {
		var data []byte
		var contentType string

		if rs, ok := f.(io.ReadSeeker); ok {
			contentType = sniffContentType(rs, filePath)
			data = make([]byte, size)
			if _, err := io.ReadFull(rs, data); err != nil {
				return err
			}
		} else {
			data = make([]byte, size)
			if _, err := io.ReadFull(f, data); err != nil {
				return err
			}
			contentType = detectContentType(filePath, data)
		}

		var etag, lastModifiedStr string
		if !modTime.IsZero() {
			etag, lastModifiedStr = strongETag(data), modTime.UTC().Format(http.TimeFormat)
		}
		cached = &cachedFile{
			data:            data,
			contentType:     contentType,
			etag:            etag,
			lastModifiedStr: lastModifiedStr,
			modTime:         modTime,
		}
		// filePath is a sub-slice of c.Path(), which on the native engines is
		// a zero-copy view over the connection's read buffer. sync.Map retains
		// the key; the buffer is reused by the next request on that conn, so
		// the stored key's bytes would mutate under the map and corrupt its
		// hash trie ("internal/sync.HashTrieMap: ran out of hash bits" panics,
		// ~67k/150s under a GET flood). Clone on the miss path only -- the hit
		// path's Load does not retain its argument.
		cache.Store(strings.Clone(filePath), cached)
	}

	// 5. Cache headers + 304 check.
	if !modTime.IsZero() {
		c.SetHeader("last-modified", cached.lastModifiedStr)
		c.SetHeader("etag", cached.etag)
		if cacheControl != "" {
			c.SetHeader("cache-control", cacheControl)
		}
		if notModified(c, cached.etag, modTime) {
			return c.NoContent(304)
		}
	}

	// 6. Serve.
	c.SetHeader("accept-ranges", "bytes")
	return serveFSCached(c, cached.data, cached.contentType, cached.etag, cached.lastModifiedStr, "")
}

// strongETag is the entity-tag of data: the first 16 bytes of its SHA-256, in
// hex, quoted. It is a strong validator (RFC 9110 §8.8.3): it changes whenever
// the bytes do, whatever the file's mtime, and it is the same for the same
// bytes in another process, so an If-Range carrying it holds only for the
// version it came from.
func strongETag(data []byte) string {
	sum := sha256.Sum256(data)
	var buf [34]byte
	buf[0] = '"'
	hex.Encode(buf[1:33], sum[:16])
	buf[33] = '"'
	return string(buf[:])
}

// computeFSCacheStrings formats the Last-Modified and weak ETag strings for a
// file with the given modTime and size. servePreCompressedFS uses it for a
// variant, which is read per request and not cached, so hashing it would cost
// its whole size each time; the cached files of serveFS get strongETag
// instead. setCacheHeaders handles the serveOS path.
func computeFSCacheStrings(modTime time.Time, size int64) (etag, lastModifiedStr string) {
	lastModifiedStr = modTime.UTC().Format(http.TimeFormat)
	var etagBuf [64]byte
	dst := append(etagBuf[:0], 'W', '/', '"')
	dst = strconv.AppendInt(dst, modTime.Unix(), 16)
	dst = append(dst, '-')
	dst = strconv.AppendInt(dst, size, 16)
	dst = append(dst, '"')
	etag = string(dst)
	return
}

// servePreCompressedFS attempts to serve a pre-compressed variant (.br or .gz)
// of the requested file from an fs.FS. The variant is the representation the
// client gets, so the Last-Modified, ETag, 304 and range are its own
// (celeris#846). Returns (true, err) if a compressed variant was served,
// (false, nil) to fall through to normal serving.
func servePreCompressedFS(c *celeris.Context, fsys fs.FS, filePath, cacheControl string) (bool, error) {
	ae := c.Header("accept-encoding")
	if ae == "" {
		return false, nil
	}

	type variant struct {
		suffix   string
		encoding string
	}
	variants := [2]variant{
		{".br", "br"},
		{".gz", "gzip"},
	}

	for _, v := range variants {
		if !strings.Contains(ae, v.encoding) {
			continue
		}
		compPath := filePath + v.suffix
		f, err := fsys.Open(compPath)
		if err != nil {
			continue
		}
		stat, statErr := f.Stat()
		if statErr != nil || stat.IsDir() || stat.Size() > maxFSFileSize {
			_ = f.Close()
			continue
		}
		// Vary goes out on the 304 too.
		addVary(c)
		var etag, lastModified string
		if modTime := stat.ModTime(); !modTime.IsZero() {
			etag, lastModified = computeFSCacheStrings(modTime, stat.Size())
			c.SetHeader("last-modified", lastModified)
			c.SetHeader("etag", etag)
			if cacheControl != "" {
				c.SetHeader("cache-control", cacheControl)
			}
			if notModified(c, etag, modTime) {
				_ = f.Close()
				return true, c.NoContent(304)
			}
		}
		data := make([]byte, stat.Size())
		_, readErr := io.ReadFull(f, data)
		_ = f.Close()
		if readErr != nil {
			return true, readErr
		}
		ct := mime.TypeByExtension(filepath.Ext(filePath))
		if ct == "" {
			// Sniff from the original (uncompressed) file if possible.
			if orig, err := fsys.Open(filePath); err == nil {
				buf := make([]byte, 512)
				n, _ := io.ReadAtLeast(orig, buf, 1)
				_ = orig.Close()
				if n > 0 {
					ct = http.DetectContentType(buf[:n])
				}
			}
			if ct == "" {
				ct = "application/octet-stream"
			}
		}
		c.SetHeader("accept-ranges", "bytes")
		return true, serveFSCached(c, data, ct, etag, lastModified, v.encoding)
	}

	return false, nil
}

// serveFSCached serves file data from the cache (or freshly read bytes),
// handling range requests via byte slicing. etag and lastModified are the
// validators serveFS put on the response ("" when the file has no modTime),
// which If-Range is checked against. The decision is the one
// Context.File makes for the Root path (internal/httprange). encoding, when
// not empty, is the Content-Encoding of data (a pre-compressed variant); a
// 416 does not carry it, as File's does not.
func serveFSCached(c *celeris.Context, data []byte, contentType, etag, lastModified, encoding string) error {
	if rng := c.Header("range"); rng != "" {
		size := int64(len(data))
		start, end, out := httprange.Decide(c.Method(), rng, c.Header("if-range"), etag, lastModified, size)
		switch out {
		case httprange.Unsatisfiable:
			var rngBuf [32]byte
			c.SetHeader("content-range", string(httprange.AppendUnsatisfied(rngBuf[:0], size)))
			return c.NoContent(http.StatusRequestedRangeNotSatisfiable)
		case httprange.Partial:
			var rngBuf [64]byte
			c.SetHeader("content-range", string(httprange.AppendContentRange(rngBuf[:0], start, end, size)))
			if encoding != "" {
				c.SetHeader("content-encoding", encoding)
			}
			return c.Blob(206, contentType, data[start:end+1])
		}
	}
	if encoding != "" {
		c.SetHeader("content-encoding", encoding)
	}
	return c.Blob(200, contentType, data)
}

// detectContentType returns the MIME type for a file. It checks the file
// extension first, then falls back to http.DetectContentType on the data.
func detectContentType(filePath string, data []byte) string {
	ct := mime.TypeByExtension(filepath.Ext(filePath))
	if ct != "" {
		return ct
	}
	if len(data) > 0 {
		return http.DetectContentType(data[:min(512, len(data))])
	}
	return "application/octet-stream"
}

// sniffContentType determines the MIME type for a file opened as an
// io.ReadSeeker. It checks the file extension first, then reads up to
// 512 bytes for content sniffing, and resets the reader position.
func sniffContentType(rs io.ReadSeeker, filePath string) string {
	ct := mime.TypeByExtension(filepath.Ext(filePath))
	if ct != "" {
		return ct
	}
	buf := make([]byte, 512)
	n, _ := io.ReadAtLeast(rs, buf, 1)
	if n > 0 {
		ct = http.DetectContentType(buf[:n])
	}
	_, _ = rs.Seek(0, io.SeekStart)
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}

// weakETag is the ETag static builds for a file from its mtime and size, W/"mtime-size"
// (hex). It is built without fmt to avoid per-request fmt.Sprintf overhead
// (formatter-scanner + arg boxing). int64 hex is <=16 chars per field, plus
// W/"...-..." framing = 37 max; 64 is a safe margin.
func weakETag(modTime time.Time, size int64) string {
	var etagBuf [64]byte
	dst := append(etagBuf[:0], 'W', '/', '"')
	dst = strconv.AppendInt(dst, modTime.Unix(), 16)
	dst = append(dst, '-')
	dst = strconv.AppendInt(dst, size, 16)
	dst = append(dst, '"')
	return string(dst)
}

// setCacheHeaders sets Last-Modified, ETag, and Cache-Control headers from
// file metadata. A zero modTime sets nothing.
//
// When chained with the etag middleware (etag → static), etag detects the
// ETag set here and reuses it as the existing tag — no double-Etag header
// is emitted. The mtime/size form static uses is preferable for static
// files (no body hash required); etag's CRC-32 fallback only runs when no
// ETag header is set.
func setCacheHeaders(c *celeris.Context, modTime time.Time, size int64, cacheControl string) {
	if modTime.IsZero() {
		return
	}
	c.SetHeader("last-modified", modTime.UTC().Format(http.TimeFormat))
	c.SetHeader("etag", weakETag(modTime, size))
	if cacheControl != "" {
		c.SetHeader("cache-control", cacheControl)
	}
}

// notModified checks If-None-Match and If-Modified-Since headers per
// RFC 7232 §6. Returns true if the client already has a fresh copy.
func notModified(c *celeris.Context, etag string, modTime time.Time) bool {
	if inm := c.Header("if-none-match"); inm != "" {
		// If-None-Match is present: check it and skip If-Modified-Since
		// regardless of whether it matches (RFC 7232 §6).
		return etagMatch(inm, etag)
	}

	if ims := c.Header("if-modified-since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil {
			if !modTime.After(t.Add(time.Second)) {
				return true
			}
		}
	}

	return false
}

// etagMatch reports whether the If-None-Match header value matches etag.
// It handles comma-separated lists and weak comparison (strips W/ prefix).
func etagMatch(inm, etag string) bool {
	if inm == "*" {
		return true
	}
	weak := func(s string) string {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "W/") {
			return s[2:]
		}
		return s
	}
	target := weak(etag)
	for _, part := range strings.Split(inm, ",") {
		if weak(part) == target {
			return true
		}
	}
	return false
}

// serveDirListingOS renders a directory listing for an OS path.
func serveDirListingOS(c *celeris.Context, dirPath string) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}
	return c.HTML(200, renderListing(c.Path(), entries))
}

// serveDirListingFS renders a directory listing for an fs.FS path.
func serveDirListingFS(c *celeris.Context, fsys fs.FS, dirPath string) error {
	entries, err := fs.ReadDir(fsys, dirPath)
	if err != nil {
		return err
	}
	return c.HTML(200, renderListing(c.Path(), entries))
}

// renderListing generates an HTML directory listing. Filenames are
// URL-encoded in hrefs to prevent protocol-scheme injection (e.g.
// javascript: filenames) and HTML-escaped in display text.
func renderListing(reqPath string, entries []fs.DirEntry) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><title>Index of ")
	b.WriteString(html.EscapeString(reqPath))
	b.WriteString("</title></head><body><h1>Index of ")
	b.WriteString(html.EscapeString(reqPath))
	b.WriteString("</h1><ul>")

	if reqPath != "/" {
		b.WriteString(`<li><a href="../">..</a></li>`)
	}

	for _, entry := range entries {
		name := entry.Name()
		displayName := html.EscapeString(name)
		encodedName := url.PathEscape(name)
		if entry.IsDir() {
			encodedName += "/"
			displayName += "/"
		}
		fmt.Fprintf(&b, `<li><a href="./%s">%s</a></li>`, encodedName, displayName)
	}

	b.WriteString("</ul></body></html>")
	return b.String()
}
