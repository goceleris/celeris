package static

import (
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

	etag := setCacheHeaders(c, info.ModTime(), info.Size(), cacheControl)
	if notModified(c, etag, info.ModTime()) {
		return c.NoContent(304)
	}

	if compress {
		if served, err := servePreCompressed(c, cleanRoot, filePath, fullPath); served {
			return err
		}
	}

	return c.FileFromDir(cleanRoot, filePath)
}

// servePreCompressed checks for .br or .gz pre-compressed variants of the
// requested file and serves them if the client accepts the encoding.
// Returns (true, err) if a pre-compressed file was served, (false, nil) to
// fall through to normal serving.
func servePreCompressed(c *celeris.Context, cleanRoot, filePath, fullPath string) (bool, error) {
	ae := c.Header("accept-encoding")
	if ae == "" {
		return false, nil
	}

	// Prefer Brotli over gzip.
	if strings.Contains(ae, "br") {
		if _, err := os.Stat(fullPath + ".br"); err == nil {
			c.SetHeader("content-encoding", "br")
			c.AddHeader("vary", "Accept-Encoding")
			return true, c.FileFromDir(cleanRoot, filePath+".br")
		}
	}
	if strings.Contains(ae, "gzip") {
		if _, err := os.Stat(fullPath + ".gz"); err == nil {
			c.SetHeader("content-encoding", "gzip")
			c.AddHeader("vary", "Accept-Encoding")
			return true, c.FileFromDir(cleanRoot, filePath+".gz")
		}
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

	// 3. Cache headers + 304 check. Reuse pre-formatted strings from the
	// fs cache when present and the modTime matches.
	modTime := stat.ModTime()
	var cached *cachedFile
	if cf, ok := cache.Load(filePath); ok {
		if cf2 := cf.(*cachedFile); cf2.modTime.Equal(modTime) {
			cached = cf2
		}
	}

	var etag, lastModifiedStr string
	if !modTime.IsZero() {
		if cached != nil {
			etag = cached.etag
			lastModifiedStr = cached.lastModifiedStr
		} else {
			etag, lastModifiedStr = computeFSCacheStrings(modTime, size)
		}
		c.SetHeader("last-modified", lastModifiedStr)
		c.SetHeader("etag", etag)
		if cacheControl != "" {
			c.SetHeader("cache-control", cacheControl)
		}
		if notModified(c, etag, modTime) {
			return c.NoContent(304)
		}
	}

	// 4. Pre-compressed check (if enabled).
	if compress {
		if served, err := servePreCompressedFS(c, fsys, filePath); served {
			return err
		}
	}

	// 5. Serve cached content if available.
	c.SetHeader("accept-ranges", "bytes")
	if cached != nil {
		return serveFSCached(c, cached.data, cached.contentType)
	}

	// 6. Read, cache (with headers), and serve.
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

	cache.Store(filePath, &cachedFile{
		data:            data,
		contentType:     contentType,
		etag:            etag,
		lastModifiedStr: lastModifiedStr,
		modTime:         modTime,
	})
	return serveFSCached(c, data, contentType)
}

// computeFSCacheStrings formats the Last-Modified and ETag strings for a
// file with the given modTime and size. Shared between the cache-miss
// path in serveFS (which stores the result on cachedFile) and one-off
// renderings; setCacheHeaders still handles the serveOS path where no
// per-file cache exists.
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
// of the requested file from an fs.FS. Returns (true, err) if a compressed
// variant was served, (false, nil) to fall through to normal serving.
func servePreCompressedFS(c *celeris.Context, fsys fs.FS, filePath string) (bool, error) {
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
		data := make([]byte, stat.Size())
		_, readErr := io.ReadFull(f, data)
		_ = f.Close()
		if readErr != nil {
			continue
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
		c.SetHeader("content-encoding", v.encoding)
		c.AddHeader("vary", "Accept-Encoding")
		return true, c.Blob(200, ct, data)
	}

	return false, nil
}

// serveFSCached serves file data from the cache (or freshly read bytes),
// handling range requests via byte slicing.
func serveFSCached(c *celeris.Context, data []byte, contentType string) error {
	if rng := c.Header("range"); rng != "" {
		if start, end, ok := parseByteRange(rng, int64(len(data))); ok {
			var rngBuf [48]byte
			dst := append(rngBuf[:0], "bytes "...)
			dst = strconv.AppendInt(dst, start, 10)
			dst = append(dst, '-')
			dst = strconv.AppendInt(dst, end, 10)
			dst = append(dst, '/')
			dst = strconv.AppendInt(dst, int64(len(data)), 10)
			c.SetHeader("content-range", string(dst))
			return c.Blob(206, contentType, data[start:end+1])
		}
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

// setCacheHeaders sets Last-Modified, ETag, and Cache-Control headers from
// file metadata. Returns the computed ETag string for reuse by notModified.
//
// When chained with the etag middleware (etag → static), etag detects the
// ETag set here and reuses it as the existing tag — no double-Etag header
// is emitted. The mtime/size form static uses is preferable for static
// files (no body hash required); etag's CRC-32 fallback only runs when no
// ETag header is set.
func setCacheHeaders(c *celeris.Context, modTime time.Time, size int64, cacheControl string) string {
	if modTime.IsZero() {
		return ""
	}
	c.SetHeader("last-modified", modTime.UTC().Format(http.TimeFormat))
	// ETag is built without fmt to avoid per-request fmt.Sprintf overhead
	// (formatter-scanner + arg boxing). int64 hex is ≤16 chars per field,
	// plus W/"…-…" framing = 37 max; 64 is a safe margin.
	var etagBuf [64]byte
	dst := append(etagBuf[:0], 'W', '/', '"')
	dst = strconv.AppendInt(dst, modTime.Unix(), 16)
	dst = append(dst, '-')
	dst = strconv.AppendInt(dst, size, 16)
	dst = append(dst, '"')
	etag := string(dst)
	c.SetHeader("etag", etag)
	if cacheControl != "" {
		c.SetHeader("cache-control", cacheControl)
	}
	return etag
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

// parseByteRange parses a "bytes=start-end" Range header value.
func parseByteRange(header string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := header[len(prefix):]
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" {
		// Suffix range: bytes=-N
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start = size - n
		end = size - 1
	} else {
		var err error
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		if parts[1] == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return 0, 0, false
			}
		}
	}
	if start < 0 || start >= size || end < start || end >= size {
		return 0, 0, false
	}
	return start, end, true
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
