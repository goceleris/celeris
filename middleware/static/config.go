package static

import (
	"io/fs"
	"time"

	"github.com/goceleris/celeris"
)

// DotfilesPolicy is what the middleware does with a request whose path below
// the root has a segment that names a hidden file or directory: one that
// starts with a dot (.env, .git, .ssh, .htaccess), except the exact segment
// ".well-known". See [Config].Dotfiles.
type DotfilesPolicy uint8

const (
	// DotfilesIgnore (the default) answers such a request as if the file did
	// not exist: the request goes to the next handler, as a path with no file
	// does (the router's 404 when nothing else answers it), so nothing tells
	// a client whether the hidden file exists. In SPA mode it gets the index
	// file, as any other missing path does.
	DotfilesIgnore DotfilesPolicy = iota

	// DotfilesAllow serves hidden files like any other: the behaviour before
	// v1.6.0.
	DotfilesAllow

	// DotfilesDeny answers such a request with 403 Forbidden, whether or not
	// the file exists.
	DotfilesDeny
)

// Config defines the static file middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Root is the directory path on the OS filesystem from which to serve
	// files. Mutually exclusive with FS. If neither Root nor FS is set,
	// the middleware panics.
	//
	// Everything below Root is served, except hidden files and directories
	// (names that start with a dot, see [Config].Dotfiles: .env and .git are not
	// served by default). A path that leaves Root, by
	// "..", an absolute path, or a symlink that resolves outside it, is
	// refused. A relative Root is read against the working directory at each
	// request; a Root made of ".." segments ("..", "../..") means the directory
	// it resolves to, and nothing outside that directory is served.
	//
	// Root may be ".", "./" or a path with ".." segments, and serves the tree
	// below it as any other directory does; "." is the process's working
	// directory, with its sources and configuration. A Root that resolves to the
	// root of a filesystem is refused: "/", "." when the working directory is "/"
	// (the default of a systemd service and of a container without a WORKDIR),
	// "../../.." reaching it, a symlink to it, a drive root such as C:\ on
	// Windows. New panics with a message that names the resolved path, unless
	// [Config].AllowFilesystemRoot is set: such a Root serves every file the
	// process can read (/etc/passwd, /proc/self/environ, keys without a dot in
	// their path), nothing is outside it, and the dotfile rule is no secrets
	// filter. The check resolves the Root when New is called; a chdir after that
	// is not covered, so resolve a relative Root to an absolute path at startup.
	// No released version served anything below such a Root, so no working
	// deployment depends on it. Before v1.6.0 "." and "./" served nothing below
	// the root either: a configuration that was inert since v1.3.3 starts to
	// serve the working directory after the upgrade (without its hidden files:
	// see [Config].Dotfiles).
	Root string

	// FS is an [fs.FS] from which to serve files (e.g. embed.FS).
	// Mutually exclusive with Root. If both Root and FS are set, FS takes
	// precedence. The middleware does not check where an FS leads: an
	// [os.DirFS] follows a symlink out of its directory, which Root does not
	// (see its containment check); use Root for a directory that may hold
	// untrusted symlinks.
	FS fs.FS

	// Prefix is the URL path prefix to strip before looking up the file.
	// For example, Prefix: "/static" serves /static/style.css from style.css
	// in Root/FS. The prefix is matched at segment boundaries: /static does
	// not match /static-docs.
	// Default: "/" (serve from the URL root).
	Prefix string

	// Index is the default file to serve when the request path resolves to
	// a directory.
	// Default: "index.html".
	Index string

	// Browse enables directory listing when the request path resolves to
	// a directory without an index file.
	// Default: false.
	Browse bool

	// SPA enables single-page application mode. When true, requests for
	// non-existent files serve the index file instead of falling through
	// to the next handler. Useful for single-page applications.
	// Default: false.
	SPA bool

	// MaxAge sets the Cache-Control max-age directive. Zero means no
	// Cache-Control header.
	MaxAge time.Duration

	// Compress enables serving pre-compressed files. When true, the middleware
	// checks for .br (Brotli) and .gz (gzip) variants of the requested file
	// and serves them if the client accepts the encoding via Accept-Encoding,
	// read with its q-values: an encoding the client refuses with q=0, by name
	// or through a "*;q=0" that does not list it ("*" matches only the
	// encodings the field does not list), is not served. Brotli is preferred over
	// gzip when both are accepted (x-gzip counts as gzip). A file served in
	// answer to a request that carries an Accept-Encoding header names
	// Accept-Encoding in Vary (once), whether or not the file has a variant.
	// Works with both OS filesystem (Root) and fs.FS.
	Compress bool

	// Dotfiles is the policy for hidden files and directories: names that
	// start with a dot (.env, .git, .ssh, .htaccess). A request whose path
	// below Root (or FS) has such a segment, after cleaning ("a/../.env" is
	// ".env") and one level of percent-decoding, is answered as the policy says:
	// [DotfilesIgnore] as if the file did not exist, whether or not it does,
	// [DotfilesDeny] with 403, [DotfilesAllow] by serving it. The exact segment
	// ".well-known" is not hidden (RFC 8615), but its children are if their own
	// names start with a dot. Only the segments of the request count: the name
	// of Root, or of the directory Root "." is in, does not. A visible name that
	// is a symlink to a hidden file or directory of the tree is refused like the
	// hidden name (for an FS, when it can resolve symlinks: an [os.DirFS] can, as
	// an [fs.ReadLinkFS]; an FS that is not one has none to follow). Browse leaves
	// hidden entries out of a listing. The Index file
	// is the operator's choice and is served even if its name starts with a dot.
	//
	// The rule is not a secrets filter: it looks at the names in the request
	// path and at where symlinks lead, nothing else. A secret whose path has no
	// dot segment (/etc/shadow readable by the process, /proc/self/environ, an
	// id_rsa, /var/run/secrets/...) is served from any Root that reaches it. And
	// it is not a sandbox against a writable tree: a symlink swapped between the
	// check and the open, and a hard link to a hidden file, defeat any path rule,
	// so the served tree must not be writable by untrusted parties.
	// The zero value is [DotfilesIgnore]; any other value than the three
	// constants panics at New.
	Dotfiles DotfilesPolicy

	// AllowFilesystemRoot acknowledges a Root that resolves to the root of a
	// filesystem ("/", "." when the working directory is "/", "../../.."
	// reaching it, a symlink to "/", a drive root such as C:\ on Windows). It
	// is refused at New, with a panic that names the resolved path, unless this
	// is true: such a Root serves every file the process can read below it, and
	// nothing is outside it any more. Default: false.
	AllowFilesystemRoot bool
}

var defaultConfig = Config{
	Prefix: "/",
	Index:  "index.html",
}

func applyDefaults(cfg Config) Config {
	if cfg.Prefix == "" {
		cfg.Prefix = defaultConfig.Prefix
	}
	if cfg.Index == "" {
		cfg.Index = defaultConfig.Index
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.Root == "" && cfg.FS == nil {
		panic("static: Root or FS must be set")
	}
	if cfg.Prefix != "" && cfg.Prefix[0] != '/' {
		panic("static: Prefix must start with /")
	}
	if cfg.Dotfiles > DotfilesDeny {
		panic("static: Dotfiles must be DotfilesIgnore, DotfilesAllow or DotfilesDeny")
	}
}
