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
// was built from, so rebuilding only the variant changes them; the 304 names
// Vary: Accept-Encoding. A request for a directory is answered with its index
// file, and with the index file's own variant.
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
