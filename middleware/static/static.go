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
	"time"

	"github.com/goceleris/celeris"
)

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
	root := cfg.Root
	fsys := cfg.FS

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
			return serveFS(c, fsys, filePath, index, browse)
		}
		return serveOS(c, root, filePath, index, browse)
	}
}

// serveOS serves a file from the OS filesystem using c.FileFromDir (which
// handles directory traversal, symlink escape, and range requests).
func serveOS(c *celeris.Context, root, filePath, index string, browse bool) error {
	fullPath := filepath.Join(root, filepath.FromSlash(filePath))

	// Prevent directory traversal: filepath.Join resolves ".." segments,
	// so verify the result stays within the root directory.
	cleanRoot := filepath.Clean(root)
	if fullPath != cleanRoot && !strings.HasPrefix(fullPath, cleanRoot+string(filepath.Separator)) {
		return c.Next()
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
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

	setCacheHeaders(c, info.ModTime(), info.Size())
	if notModified(c, info.ModTime(), info.Size()) {
		return c.NoContent(304)
	}

	return c.FileFromDir(root, filePath)
}

// serveFS serves a file from an fs.FS with ETag/Last-Modified and range support.
func serveFS(c *celeris.Context, fsys fs.FS, filePath, index string, browse bool) error {
	// fs.FS uses "." for the root directory.
	openPath := filePath
	if openPath == "" {
		openPath = "."
	}

	f, err := fsys.Open(openPath)
	if err != nil {
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
				return serveFS(c, fsys, indexPath, index, browse)
			}
		}
		if browse {
			return serveDirListingFS(c, fsys, openPath)
		}
		return c.Next()
	}

	size := stat.Size()
	if size > maxFSFileSize {
		return celeris.NewHTTPError(413, "file exceeds 100MB limit")
	}

	modTime := stat.ModTime()
	if !modTime.IsZero() {
		setCacheHeaders(c, modTime, size)
		if notModified(c, modTime, size) {
			return c.NoContent(304)
		}
	}

	contentType := mime.TypeByExtension(filepath.Ext(filePath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	c.SetHeader("accept-ranges", "bytes")

	if rng := c.Header("range"); rng != "" {
		if start, end, ok := parseByteRange(rng, int64(len(data))); ok {
			c.SetHeader("content-range", fmt.Sprintf("bytes %d-%d/%d", start, end, int64(len(data))))
			return c.Blob(206, contentType, data[start:end+1])
		}
	}

	return c.Blob(200, contentType, data)
}

// setCacheHeaders sets Last-Modified and ETag headers from file metadata.
func setCacheHeaders(c *celeris.Context, modTime time.Time, size int64) {
	if modTime.IsZero() {
		return
	}
	c.SetHeader("last-modified", modTime.UTC().Format(http.TimeFormat))
	c.SetHeader("etag", fmt.Sprintf(`W/"%x-%x"`, modTime.Unix(), size))
}

// notModified checks If-None-Match and If-Modified-Since headers.
// Returns true if the client already has a fresh copy.
func notModified(c *celeris.Context, modTime time.Time, size int64) bool {
	etag := fmt.Sprintf(`W/"%x-%x"`, modTime.Unix(), size)

	if inm := c.Header("if-none-match"); inm == etag {
		return true
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
