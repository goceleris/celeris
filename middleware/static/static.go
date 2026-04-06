package static

import (
	"fmt"
	"html"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/goceleris/celeris"
)

// New creates a static file middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	root := cfg.Root
	fsys := cfg.Filesystem
	index := cfg.Index
	spa := cfg.SPA
	browse := cfg.Browse
	maxAge := cfg.MaxAge
	prefix := cfg.Prefix

	var cacheVal string
	if maxAge > 0 {
		cacheVal = "public, max-age=" + strconv.Itoa(maxAge)
	}

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		m := c.Method()
		if m != "GET" && m != "HEAD" {
			return c.Next()
		}

		reqPath := c.Path()

		// Strip prefix.
		if prefix != "/" {
			if !strings.HasPrefix(reqPath, prefix) {
				return c.Next()
			}
			reqPath = reqPath[len(prefix):]
			if reqPath == "" {
				reqPath = "/"
			}
		}

		// Sanitize the file path.
		filePath := sanitize(reqPath)
		if filePath == "" {
			return c.Next()
		}

		if cacheVal != "" {
			c.SetHeader("cache-control", cacheVal)
		}

		// Try serving the file.
		if root != "" {
			return serveOS(c, root, filePath, index, spa, browse)
		}
		return serveFS(c, fsys, filePath, index, spa, browse)
	}
}

// sanitize cleans a URL path for file lookup. Returns "" if the path is
// unsafe (contains ..).
func sanitize(p string) string {
	p = path.Clean(p)
	// path.Clean preserves a leading / — strip it for file operations.
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		p = "."
	}
	// Reject any remaining ".." component.
	if containsDotDot(p) {
		return ""
	}
	return p
}

// containsDotDot reports whether s contains a ".." path segment.
func containsDotDot(s string) bool {
	for {
		i := strings.Index(s, "..")
		if i < 0 {
			return false
		}
		// Check that ".." is bounded by separators or string edges.
		if (i == 0 || s[i-1] == '/') && (i+2 == len(s) || s[i+2] == '/') {
			return true
		}
		s = s[i+2:]
	}
}

func serveOS(c *celeris.Context, root, filePath, index string, spa, browse bool) error {
	// Use FileFromDir for path traversal + symlink protection.
	err := c.FileFromDir(root, filePath)
	if err == nil {
		return nil
	}

	// Check if the target is a directory (try index).
	fullPath := root + "/" + filePath
	info, statErr := os.Stat(fullPath)
	if statErr == nil && info.IsDir() {
		// Try serving the index file.
		indexPath := filePath + "/" + index
		if filePath == "." {
			indexPath = index
		}
		if err2 := c.FileFromDir(root, indexPath); err2 == nil {
			return nil
		}
		if browse {
			return browseOS(c, fullPath)
		}
	}

	if spa {
		if err2 := c.FileFromDir(root, index); err2 == nil {
			return nil
		}
	}

	// Clear cache-control if we're falling through.
	c.SetHeader("cache-control", "")
	return c.Next()
}

func serveFS(c *celeris.Context, fsys fs.FS, filePath, index string, spa, browse bool) error {
	// Try opening the file directly.
	f, err := fsys.Open(filePath)
	if err == nil {
		stat, statErr := f.Stat()
		_ = f.Close()
		if statErr == nil {
			if !stat.IsDir() {
				return c.FileFromFS(filePath, fsys)
			}
			// It's a directory — try the index.
			indexPath := filePath + "/" + index
			if filePath == "." {
				indexPath = index
			}
			if err2 := c.FileFromFS(indexPath, fsys); err2 == nil {
				return nil
			}
			if browse {
				return browseFS(c, fsys, filePath)
			}
		}
	} else {
		_ = f // nil, Open failed
	}

	if spa {
		if err2 := c.FileFromFS(index, fsys); err2 == nil {
			return nil
		}
	}

	// Clear cache-control if we're falling through.
	c.SetHeader("cache-control", "")
	return c.Next()
}

func browseOS(c *celeris.Context, dirPath string) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return c.Next()
	}
	return renderListing(c, entries)
}

func browseFS(c *celeris.Context, fsys fs.FS, filePath string) error {
	entries, err := fs.ReadDir(fsys, filePath)
	if err != nil {
		return c.Next()
	}
	return renderListing(c, entries)
}

func renderListing(c *celeris.Context, entries []fs.DirEntry) error {
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>Directory listing</title></head><body><ul>\n")
	for _, e := range entries {
		name := e.Name()
		escaped := html.EscapeString(name)
		if e.IsDir() {
			fmt.Fprintf(&b, "<li><a href=\"%s/\">%s/</a></li>\n", escaped, escaped)
		} else {
			fmt.Fprintf(&b, "<li><a href=\"%s\">%s</a></li>\n", escaped, escaped)
		}
	}
	b.WriteString("</ul></body></html>")
	return c.HTML(200, b.String())
}
