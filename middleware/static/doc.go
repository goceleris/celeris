// Package static serves static files from the OS filesystem or an [fs.FS].
//
// Use [New] with a [Config] to mount the middleware. Set [Config].Root for an
// OS directory or [Config].FS for an embedded/virtual filesystem (mutually
// exclusive; FS takes precedence when both are set). Key options:
//
//   - [Config].Prefix — URL path prefix, matched at segment boundaries.
//   - [Config].Index — directory index filename (default "index.html").
//   - [Config].Browse — enable HTML directory listings.
//   - [Config].SPA — single-page app mode: unknown paths serve the index file.
//   - [Config].MaxAge — Cache-Control max-age duration (zero = no header).
//   - [Config].Compress — serve pre-compressed .br/.gz variants when accepted.
//   - [Config].Dotfiles — hidden files and directories (names that start with
//     a dot) are not served by default.
//   - [Config].Skip / [Config].SkipPaths — skip the middleware dynamically or
//     by exact path match.
//
// Only GET and HEAD requests are processed; all others pass through. The
// middleware sets Last-Modified, ETag and optional Cache-Control headers and
// handles conditional requests (304 Not Modified). With [Config].Root the
// ETag is weak (mtime+size). With [Config].FS the ETag of a file is strong, a
// hash of the file's bytes taken when the file is first read (so a 304 for a
// file not yet read reads it first), and a file with a zero ModTime, such as
// one from an embed.FS, gets neither header. A pre-compressed variant served
// from an FS is read for each request and not hashed: it keeps the weak
// mtime+size ETag. With [Config].Compress the validators, the 304 and the
// range are those of the .br/.gz variant served, not of the original file it
// was built from, so rebuilding only the variant changes them; a file served,
// or answered with a 304, for a request with an Accept-Encoding header names
// Vary: Accept-Encoding, whether or not the file has a variant. A request for
// a directory is answered with its index file, and with the index file's own
// variant. With Root, a conditional request is answered 304 only after a
// containment check, the one [celeris.Context.FileFromDir] applies to a file it
// sends (symlinks resolved), has passed for its path, and that check runs for
// every conditional request, whether or not its validator matches. The two
// checks agree, for a Root of ".", "./" as for any other directory: these
// serve the tree below them (before v1.6.0 they served nothing below the root,
// and the root's own index answered 400 or 304 depending on whether a validator
// matched), so a configuration that was inert since v1.3.3 starts to serve the
// working directory after the upgrade, without its hidden files (below). A Root
// made of ".." segments means the directory it resolves to, and nothing outside
// it is served.
//
// A Root that resolves to the root of a filesystem is refused: "/", "." when
// the working directory is "/" (a systemd service, a container without a
// WORKDIR), "../../.." reaching it, a symlink to it, a drive root on Windows.
// New panics, naming the resolved path, unless [Config].AllowFilesystemRoot is
// set; Server.Static and Group.Static refuse it with no way to acknowledge, so
// serve a filesystem root on purpose with this middleware. The Root is
// resolved when New is called: a chdir after that is not covered, so resolve a
// relative Root to an absolute path at startup. The Root should exist then; a
// Root that is or contains a symlink is followed at each request, so a later
// retarget to "/" is not refused. Devices, FIFOs and sockets are
// never served: they are answered as a missing file, since reading /dev/zero
// never ends and opening a FIFO waits for a writer. Name the directory that holds
// what is public; see [Config].Root.
//
// # Dotfiles
//
// A file or directory whose name starts with a dot (.env, .git, .ssh,
// .htaccess) is not served by default: a request whose path below the root
// has such a segment is answered as if the file did not exist ([DotfilesIgnore]:
// the next handler, which is the router's 404 when nothing else answers; the
// index file in SPA mode), whether or not it exists, so a client learns
// nothing about it. Every segment of the request path counts, after the path
// is cleaned ("a/../.env" is ".env") and with one level of percent-decoding;
// the name of the root does not: a Root that is ".config", or "." in such a
// directory, serves its files. The rule also follows a symlink: a visible name
// that leads to a hidden file or directory of the tree is refused like it. Under
// a [Config].FS (an os.DirFS, or an fs.Sub of one) the links are followed while
// they stay inside the FS; links that leave the FS (an absolute target, or one
// that climbs above its root) are not followed under Ignore and Deny: such a
// path is refused like a hidden one, because the FS cannot say what is beyond
// it. The
// exact segment ".well-known" is served (RFC 8615: ACME challenges,
// security.txt, apple-app-site-association), but its children are served only
// if they are not hidden themselves. A directory listing (Browse) leaves the
// hidden entries out. The Index file is the operator's choice and is served
// even if its name starts with a dot. [DotfilesDeny] answers 403 instead;
// [DotfilesAllow] serves hidden files like any other, as before v1.6.0:
//
//	// A project that serves a hidden file on purpose names its directory and
//	// allows dotfiles there, and keeps its secrets out of that directory.
//	static.New(static.Config{Root: "./public", Dotfiles: static.DotfilesAllow})
//
// The rule is not a secrets filter. It looks at the names in the request path
// and at where symlinks lead, and nothing else: a secret whose path has no dot
// segment (/etc/shadow when the process can read it, /proc/self/environ, an
// id_rsa, /var/run/secrets/...) is served from any Root that reaches it, and
// that is what an acknowledged filesystem root does. Nor is it a sandbox for a
// tree that others can write to: a symlink swapped between the check and the
// open, and a hard link to a hidden file, defeat any rule about paths. Keep
// secrets out of the served directory, and the directory out of untrusted hands.
//
// The rule is the middleware's, and that of Server.Static and Group.Static,
// which serve a directory too (without a way to opt out: use the middleware for
// that). [celeris.Context.FileFromDir], whose caller chooses the path, does not
// apply it.
//
// With compress in front of an [Config].FS, the strong tag of a file that compress
// encodes is made weak on the 200 (W/"hash"), and static marks the response so
// that compress makes the 304 of a revalidation carry that same tag (RFC 9110
// 15.4.5); see the compress package. Root's weak tag needs no mark.
//
// A GET with a single byte range gets 206 Partial Content; a range no byte of
// the file satisfies gets 416 Range Not Satisfiable with
// "Content-Range: bytes */<size>" (the size of the variant, for a pre-compressed
// one). If-Range is honoured: the range is served only while the client's
// validator still matches, otherwise the whole file is sent as a 200. The
// strong comparison applies (RFC 9110 §13.1.5), so with Root, whose ETag is
// weak, only the Last-Modified date can match; with FS the ETag of the
// identity file can (that of a pre-compressed variant is weak, so its date
// does). A date is only as reliable as the files' mtimes: build tools that
// normalise them (ko, Nix, Bazel rules_oci, SOURCE_DATE_EPOCH) give every
// version of a file the same date, and an If-Range carrying it then matches a
// changed file. A resume is safe only when the validator changes with the
// bytes served.
// HEAD ignores Range, and a request for several ranges gets the whole file.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/static-files
package static
