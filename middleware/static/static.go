package static

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	// rep is what a response marks itself with for compress (see
	// markCompressible). The mark is a pointer to this field, not to the
	// cachedFile: the mark lands in the context's keys, which Context.Keys
	// hands to user code, and a middleware that prints every value would
	// print data, up to maxFSFileSize bytes.
	rep representation
}

// representation is the mark: the size and content type of the identity bytes
// of a file. compress asks it for them (CompressibleRepresentation): a 304 has
// neither, and compress decides on them whether it would have encoded the 200
// (celeris#888 item 36).
type representation struct {
	size        int
	contentType string
}

// CompressibleRepresentation answers middleware/compress.
func (r *representation) CompressibleRepresentation() (size int, contentType string) {
	return r.size, r.contentType
}

// compressibleKey is the context key under which static, and middleware/etag,
// mark a response whose strong ETag is a hash of the identity bytes, for
// middleware/compress (celeris#888 item 36): it weakens that tag on the 200 it
// encodes, and needs the mark to give the 304 the tag the 200 carries (RFC 9110
// 15.4.5). compress has its own copy of the string. The key is part of no API.
const compressibleKey = "celeris.compressible-representation"

// compressActiveKey is the key compress sets when it has negotiated an encoding
// for the request and buffers the response (compress has its own copy).
const compressActiveKey = "celeris.compress.active"

// markCompressible marks the response for f, which carries the strong tag etag,
// when compress in front negotiated an encoding for the request (it sets
// compressActiveKey: without it nothing weakens the tag, and the mark would
// cost a map store for nothing), and the answer would be the whole file, not a
// 206 or a 416, which compress sends as they are, so that their 304 keeps the
// strong tag too (Range, and If-Range against the tag and date, decide that
// as the 200 does). A weak tag, which compress does not change, is not marked.
func markCompressible(c *celeris.Context, f *cachedFile, etag, lastModified string) {
	if etag == "" || etag[0] == 'W' {
		return
	}
	if _, ok := c.Get(compressActiveKey); !ok {
		return
	}
	if rng := c.Header("range"); rng != "" {
		if _, _, out := httprange.Decide("GET", rng, c.Header("if-range"), etag, lastModified, int64(len(f.data))); out != httprange.Full {
			return
		}
	}
	c.Set(compressibleKey, &f.rep)
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
	dotfiles := cfg.Dotfiles

	// hidden is nil when hidden files are served (DotfilesAllow). Otherwise it
	// tells whether a slash path below the root, resolved, has a hidden
	// segment; the Index file, which the operator chose, is not counted.
	var hidden func(rel string) bool
	if dotfiles != DotfilesAllow {
		idx := strings.Trim(filepath.ToSlash(filepath.Clean(index)), "/")
		hidden = func(rel string) bool {
			for _, v := range [...]string{"", ".br", ".gz"} {
				if t := idx + v; rel == t || strings.HasSuffix(rel, "/"+t) {
					rel = strings.TrimSuffix(strings.TrimSuffix(rel, t), "/")
					break
				}
			}
			return ctxkit.HiddenPath(rel)
		}
	}

	// Precompute cleanRoot once instead of per request.
	var cleanRoot string
	if root != "" {
		// A root made of ".." segments is resolved against the working
		// directory here, once (see ctxkit.ResolveClimbingRoot).
		cleanRoot = ctxkit.ResolveClimbingRoot(filepath.Clean(root))
		// A root that resolves to the root of a filesystem serves every file the
		// process can read: refused unless acknowledged. The working directory
		// and the symlinks are read now; a chdir after this is not covered.
		if fsys == nil && !cfg.AllowFilesystemRoot {
			if resolved, ok := ctxkit.FilesystemRoot(cleanRoot); ok {
				panic(fmt.Sprintf("static: Root %q resolves to the filesystem root %q, which serves every file the process can read: "+
					"name the directory that holds the public files, or set Config.AllowFilesystemRoot to serve a filesystem root on purpose "+
					"(the working directory is read now: a later chdir is not covered)", root, resolved))
			}
		}
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

		// notFound answers a hidden path as a path with no file is answered:
		// the index in SPA mode, the next handler otherwise (DotfilesIgnore), or
		// refuses it (DotfilesDeny). A hidden path is decided before the file
		// system is looked at, so what is answered does not tell whether it exists.
		missing := func() error {
			switch {
			case spa && fsys != nil:
				return serveFS(c, fsys, index, index, browse, false, compress, cacheControl, &fsCache, hidden)
			case spa:
				return c.FileFromDir(cleanRoot, index)
			}
			return c.Next()
		}
		notFound := func() error {
			if dotfiles == DotfilesDeny {
				return celeris.NewHTTPError(403, "Forbidden")
			}
			return missing()
		}
		if hidden != nil && ctxkit.HiddenPath(filePath) {
			return notFound()
		}

		var err error
		if fsys != nil {
			err = serveFS(c, fsys, filePath, index, browse, spa, compress, cacheControl, &fsCache, hidden)
		} else {
			err = serveOS(c, cleanRoot, filePath, index, browse, spa, cacheControl, compress, hidden)
		}
		switch {
		case err == nil:
		case errors.Is(err, ctxkit.ErrHidden):
			return notFound()
		case errors.Is(err, ctxkit.ErrNotRegular):
			// a device, a FIFO or a socket is answered as a missing file is
			return missing()
		}
		return err
	}
}

// serveOS serves a file from the OS filesystem using c.FileFromDir (which
// handles directory traversal, symlink escape, and range requests).
// cleanRoot must be filepath.Clean(root) — precomputed by New.
func serveOS(c *celeris.Context, cleanRoot, filePath, index string, browse, spa bool, cacheControl string, compress bool, hidden func(rel string) bool) error {
	fullPath := filepath.Join(cleanRoot, filepath.FromSlash(filePath))

	// Prevent directory traversal: filepath.Join resolves ".." segments,
	// so verify the result stays within the root directory.
	if !ctxkit.PathWithin(cleanRoot, fullPath) {
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
		if indexErr == nil && indexInfo.Mode().IsRegular() {
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
			if !ctxkit.ResolvedWithin(resolvedRoot, resolved) {
				return c.Next()
			}
			if hidden != nil {
				// a visible name that leads to a hidden directory is not listed
				if rel, ok := ctxkit.RelWithin(resolvedRoot, resolved); !ok || hidden(rel) {
					return ctxkit.ErrHidden
				}
			}
			return serveDirListingOS(c, resolved, hidden != nil)
		} else {
			return c.Next()
		}
	}

	if !info.Mode().IsRegular() {
		// a device (/dev/zero reads without end), a FIFO (open waits for a
		// writer), a socket: not served, answered as a file that is not there
		if spa {
			return c.FileFromDir(cleanRoot, index)
		}
		return c.Next()
	}

	if serveOSHook != nil {
		serveOSHook()
	}

	// The validators of a response that sends the file come from the file as
	// it is opened for serving (serveOSFile), and from the pre-compressed
	// variant when one is served; this stat only decided that there is a file,
	// and answers a 304.
	if compress {
		if served, err := servePreCompressed(c, cleanRoot, filePath, fullPath, cacheControl, hidden); served {
			return err
		}
	}

	return serveOSFile(c, cleanRoot, filePath, "", "", cacheControl, info, hidden)
}

// serveOSHook is a test seam (celeris#846 item 4): when set it runs in serveOS
// after the path was stat-ed and before the file is served, which is where
// the path used to be replaced under the validators.
var serveOSHook func()

// serveOSFile serves filePath under cleanRoot through Context.FileFromDir.
// early is a stat of the path made just before.
//
// A conditional request is answered from early when it holds and the path
// resolves inside the root (resolvesInside): a 304 sends no body, so there is
// nothing for a replaced file to contradict, and it costs one stat and one
// Lstat per path element, where FileFromDir resolves symlinks twice and opens
// the file. A path that leaves the root is not answered from its target's
// stat: it goes to FileFromDir, which refuses it. The containment check runs
// for every conditional request, before the validators are compared, so a
// refusal takes the same time whether or not the client guessed the target's
// mtime and size. Every response that sends the file gets its Last-Modified,
// ETag and Cache-Control from the mtime and size of the file as it is opened:
// early could describe another file than the one opened when the path is
// replaced in between (celeris#846). So the Range and If-Range that File then
// decides are about the very bytes sent.
//
// contentType, when not empty, is the response's type (a pre-compressed
// variant is served with its original's, not the one its ".gz" maps to).
// encoding, when not empty, is set as Content-Encoding on the response that
// carries the file, not on a 304; File drops it again from a 416.
func serveOSFile(c *celeris.Context, cleanRoot, filePath, contentType, encoding, cacheControl string, early os.FileInfo, hidden func(rel string) bool) error {
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
		// The 304 is about a file inside the root: a path that resolves
		// outside it is not answered from its target's stat but handed to
		// FileFromDir, which refuses it (celeris#888). The check comes first
		// and runs for every conditional request, so what it costs does not
		// depend on whether the client's validator matches the target.
		if resolvesInside(cleanRoot, filePath, hidden) && notModified(c, etag, early.ModTime()) {
			if etag != "" {
				setValidators(c, early.ModTime(), etag, cacheControl)
			}
			return c.NoContent(304)
		}
	}
	return ctxkit.FileFromDir(c, cleanRoot, filePath, contentType, func(modTime time.Time, size int64) {
		setCacheHeaders(c, modTime, size, cacheControl)
		if encoding != "" {
			c.SetHeader("content-encoding", encoding)
		}
	}, hidden)
}

// resolvesInside reports whether filePath, a slash path below cleanRoot, still
// names a place inside cleanRoot once its symlinks are resolved: the check
// Context.FileFromDir makes before it opens a file, ahead of a 304 that is
// answered without opening it.
//
// The common path has no symlink, and one Lstat per path element below the
// root settles that (the root's own path is not looked at: a symlinked root
// is the operator's choice, and FileFromDir resolves it too). A symlink, or
// an irregular element (a Windows junction), or an Lstat error, falls to
// FileFromDir's own resolution, which costs two EvalSymlinks. A path that is
// lexically outside the root (Config.Index is joined to the requested
// directory unchecked) is refused without a syscall. The answer can change
// before the 304 is sent, as it can between FileFromDir's check and its open.
func resolvesInside(cleanRoot, filePath string, hidden func(rel string) bool) bool {
	if resolvesInsideHook != nil {
		resolvesInsideHook()
	}
	full := filepath.Join(cleanRoot, filepath.FromSlash(filePath))
	if !ctxkit.PathWithin(cleanRoot, full) {
		return false
	}
	start := len(cleanRoot)
	if cleanRoot == "." {
		start = 0 // Join drops a leading ".", so full has no root prefix
	}
	linkFree := true
	for off := start; off < len(full); {
		if full[off] == filepath.Separator {
			off++
			continue
		}
		end := len(full)
		if i := strings.IndexByte(full[off:], filepath.Separator); i >= 0 {
			end = off + i
		}
		fi, err := os.Lstat(full[:end])
		if err != nil || fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			linkFree = false
			break
		}
		off = end
	}
	if linkFree && len(full) > start {
		return true
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return false
	}
	if !ctxkit.ResolvedWithin(resolvedRoot, resolved) {
		return false
	}
	if hidden != nil {
		// a symlink that leads to a hidden file is no 304 oracle either
		rel, ok := ctxkit.RelWithin(resolvedRoot, resolved)
		return ok && !hidden(rel)
	}
	return true
}

// resolvesInsideHook is a test seam (celeris#888 item 35): when set it runs at
// the top of resolvesInside, so a test can count the containment checks a
// conditional request pays.
var resolvesInsideHook func()

// acceptsCoding reports whether the request's Accept-Encoding accepts the
// content coding enc (q-values read, q=0 refuses), and names Accept-Encoding
// in Vary, once. A client that accepts x-gzip accepts gzip (RFC 9110 8.4.1.3:
// x-gzip is equivalent), and the variant is sent as "gzip".
func acceptsCoding(c *celeris.Context, enc string) bool {
	if enc == "gzip" {
		return c.AcceptsEncodings("gzip", "x-gzip") != ""
	}
	return c.AcceptsEncodings(enc) != ""
}

// servePreCompressed checks for .br or .gz pre-compressed variants of the
// requested file and serves them if the client accepts the encoding. The
// variant is the representation the client gets and keeps, so its validators,
// 304, If-Range and Content-Range are its own (celeris#846), and it is served
// with the original's content type. Returns (true, err) if a pre-compressed
// file was served, (false, nil) to fall through to normal serving.
func servePreCompressed(c *celeris.Context, cleanRoot, filePath, fullPath, cacheControl string, hidden func(rel string) bool) (bool, error) {
	if c.Header("accept-encoding") == "" {
		return false, nil
	}

	// Prefer Brotli over gzip.
	for _, v := range [...]struct{ suffix, encoding string }{{".br", "br"}, {".gz", "gzip"}} {
		// The client's q-values decide (br;q=0 refuses br), and the
		// negotiation names Accept-Encoding in Vary, once, whichever
		// representation is sent, a 304 included.
		if !acceptsCoding(c, v.encoding) {
			continue
		}
		vi, err := os.Stat(fullPath + v.suffix)
		if err != nil || !vi.Mode().IsRegular() {
			continue
		}
		ct := mime.TypeByExtension(filepath.Ext(filePath))
		if ct == "" {
			ct = "application/octet-stream"
		}
		err = serveOSFile(c, cleanRoot, filePath+v.suffix, ct, v.encoding, cacheControl, vi, hidden)
		if errors.Is(err, ctxkit.ErrHidden) || errors.Is(err, ctxkit.ErrNotRegular) {
			// a variant that is a symlink to a hidden file, or not a regular
			// file, is no variant: the file itself is served
			continue
		}
		return true, err
	}

	return false, nil
}

// resolveFSLinks returns name, a slash path below the FS root, with every
// symlink on it replaced by its target, as the operating system would walk it:
// the path the file is really at, for the hidden check. A link whose target is
// absolute leaves the FS: the result is then the target, with the rest of the
// path after it, and its own segments are what counts. A path that climbs above
// the FS root keeps its "..", which the check cleans away. More than 40 links
// on the way (a loop) resolve to a hidden path: refused.
func resolveFSLinks(rl fs.ReadLinkFS, name string) string {
	queue := strings.Split(name, "/")
	var resolved []string
	for follows := 0; len(queue) > 0; {
		seg := queue[0]
		queue = queue[1:]
		switch seg {
		case "", ".":
			continue
		case "..":
			if n := len(resolved); n > 0 && resolved[n-1] != ".." {
				resolved = resolved[:n-1]
			} else {
				resolved = append(resolved, "..")
			}
			continue
		}
		cand := strings.Join(append(resolved, seg), "/")
		fi, err := rl.Lstat(cand)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			resolved = append(resolved, seg)
			continue
		}
		if follows++; follows > 40 {
			return ".loop"
		}
		target, err := rl.ReadLink(cand)
		if err != nil {
			return ".unreadable-link"
		}
		target = filepath.ToSlash(target)
		if strings.HasPrefix(target, "/") || filepath.IsAbs(target) {
			return target + "/" + strings.Join(queue, "/")
		}
		queue = append(strings.Split(target, "/"), queue...)
	}
	return strings.Join(resolved, "/")
}

// serveFS serves a file from an fs.FS with ETag/Last-Modified and range support.
func serveFS(c *celeris.Context, fsys fs.FS, filePath, index string, browse, spa, compress bool, cacheControl string, cache *sync.Map, hidden func(rel string) bool) error {
	// 1. Open + stat + directory handling.
	openPath := filePath
	if openPath == "" {
		openPath = "."
	}

	// Where the path leads, for an FS that can resolve symlinks (an os.DirFS):
	// a visible name that links to a hidden file or directory is refused like
	// the hidden name (an FS that cannot, an embed.FS or an fstest.MapFS
	// without links, has none to follow).
	if hidden != nil && filePath != "" {
		if rl, ok := fsys.(fs.ReadLinkFS); ok && hidden(resolveFSLinks(rl, filePath)) {
			return ctxkit.ErrHidden
		}
	}
	// Before the open: for an os.DirFS, open(2) of a FIFO waits for a writer.
	if _, ok := fsys.(fs.StatFS); ok {
		if info, err := fs.Stat(fsys, openPath); err == nil && !info.IsDir() && !info.Mode().IsRegular() {
			return ctxkit.ErrNotRegular
		}
	}

	f, err := fsys.Open(openPath)
	if err != nil {
		if spa {
			return serveFS(c, fsys, index, index, browse, false, compress, cacheControl, cache, hidden)
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
				return serveFS(c, fsys, indexPath, index, browse, spa, compress, cacheControl, cache, hidden)
			}
		}
		if browse {
			return serveDirListingFS(c, fsys, openPath, hidden != nil)
		}
		return c.Next()
	}

	if !stat.Mode().IsRegular() {
		return ctxkit.ErrNotRegular
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
		if served, err := servePreCompressedFS(c, fsys, filePath, cacheControl, hidden); served {
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
			rep:             representation{size: len(data), contentType: contentType},
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
		markCompressible(c, cached, cached.etag, cached.lastModifiedStr)
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
func servePreCompressedFS(c *celeris.Context, fsys fs.FS, filePath, cacheControl string, hidden func(rel string) bool) (bool, error) {
	if c.Header("accept-encoding") == "" {
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
		if !acceptsCoding(c, v.encoding) {
			continue
		}
		compPath := filePath + v.suffix
		if hidden != nil {
			// a variant that is a symlink to a hidden file is no variant
			if rl, ok := fsys.(fs.ReadLinkFS); ok && hidden(resolveFSLinks(rl, compPath)) {
				continue
			}
		}
		if _, ok := fsys.(fs.StatFS); ok {
			if info, err := fs.Stat(fsys, compPath); err != nil || !info.Mode().IsRegular() {
				continue
			}
		}
		f, err := fsys.Open(compPath)
		if err != nil {
			continue
		}
		stat, statErr := f.Stat()
		if statErr != nil || !stat.Mode().IsRegular() || stat.Size() > maxFSFileSize {
			_ = f.Close()
			continue
		}
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
	setValidators(c, modTime, weakETag(modTime, size), cacheControl)
}

// setValidators sets Last-Modified, the given ETag and Cache-Control; a 304
// that has built the tag to compare with it sets that same string.
func setValidators(c *celeris.Context, modTime time.Time, etag, cacheControl string) {
	c.SetHeader("last-modified", modTime.UTC().Format(http.TimeFormat))
	c.SetHeader("etag", etag)
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
func serveDirListingOS(c *celeris.Context, dirPath string, hide bool) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}
	if hide {
		entries = withoutHidden(entries)
	}
	return c.HTML(200, renderListing(c.Path(), entries))
}

// serveDirListingFS renders a directory listing for an fs.FS path.
func serveDirListingFS(c *celeris.Context, fsys fs.FS, dirPath string, hide bool) error {
	entries, err := fs.ReadDir(fsys, dirPath)
	if err != nil {
		return err
	}
	if hide {
		entries = withoutHidden(entries)
	}
	return c.HTML(200, renderListing(c.Path(), entries))
}

// withoutHidden drops the entries whose name is hidden (starts with a dot,
// except ".well-known"). It filters in place: the slice is the caller's own.
func withoutHidden(entries []fs.DirEntry) []fs.DirEntry {
	kept := entries[:0]
	for _, e := range entries {
		if !ctxkit.HiddenSegment(e.Name()) {
			kept = append(kept, e)
		}
	}
	return kept
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
