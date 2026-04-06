# Security Policy

## Supported Versions

| Version        | Supported          |
|----------------|--------------------|
| >= 1.3.0       | Yes                |
| < 1.3.0        | No                 |

Only the latest minor release receives security patches. Upgrade to the latest version to ensure you have all fixes.

### v1.3.3 Security Improvements

v1.3.3 includes security-hardened defaults in three new middleware packages:

- **Pprof loopback-only default**: The pprof middleware restricts access to loopback IPs (127.0.0.1, ::1) by default. Exposing Go profiling data to untrusted networks reveals goroutine stacks, heap contents, and source code paths.
- **Static path traversal protection**: The static middleware uses celeris's FileFromDir (with symlink resolution and prefix verification) for OS filesystem access. For fs.FS, paths are cleaned and .. components are rejected.
- **Swagger path-only interception**: The swagger middleware only responds to exact path matches under its BasePath ({BasePath}/, {BasePath}/spec) and ignores all other requests. OpenAPI specs may reveal internal API structure — restrict access with upstream auth middleware or network-level controls.

### v1.3.2 Security Improvements

v1.3.2 includes a security fix and hardening in the new resilience middleware:

- **Singleflight cross-user data leakage (fixed)**: The default deduplication key now includes `Authorization` and `Cookie` request headers. Without this, concurrent authenticated requests to the same endpoint would be coalesced, leaking one user's response (including PII, session cookies) to another user. Custom `KeyFunc` implementations must incorporate user identity for authenticated endpoints.
- **Singleflight multi-value header replay (fixed)**: Waiter response replay now uses `SetResponseHeaders` (bulk replace) instead of a `SetHeader` loop, preserving multi-value headers like `Set-Cookie`. The previous implementation dropped all but the last value for duplicate header keys.
- **Circuit breaker panic recording**: Handler panics are now recorded as failures in the sliding window via `defer/recover` before re-panicking. Previously, panics bypassed the circuit breaker entirely, making it blind to panic-heavy failure modes.
- **Circuit breaker validation hardened**: `validate()` now rejects negative `CooldownPeriod`, `HalfOpenMax < 1`, `WindowSize < 10ms`, and `MinRequests < 1`. Previously only `Threshold` was validated, allowing configurations that caused division-by-zero panics or permanently stuck breakers.

### v1.3.1 Security Improvements

v1.3.1 includes security hardening in the new HTTP transport middleware and core:

- **Scheme() trust model**: `Context.Scheme()` no longer reads `X-Forwarded-Proto` from untrusted clients. Only the proxy middleware (which validates against `TrustedProxies`) can set the scheme override. This prevents `HTTPSRedirect` bypass by direct-connect attackers.
- **Proxy host validation**: `X-Forwarded-Host` is validated against CRLF injection, null bytes, path traversal (`/`, `\`), query injection (`?`, `#`), userinfo injection (`@`), and a 253-byte DNS length cap.
- **Proxy IP validation**: `X-Real-IP` and custom headers (e.g., `CF-Connecting-IP`) are parsed with `netip.ParseAddr` — non-IP values are rejected.
- **Method override target restriction**: `TargetMethods` whitelist (default: PUT, DELETE, PATCH) prevents overriding POST to arbitrary methods like CONNECT or TRACE.
- **Negotiate q=0 exclusion**: `Accept-Encoding` entries with `q=0` are now correctly treated as "not acceptable" per RFC 9110, including wildcard exclusions (e.g., `*, br;q=0` excludes brotli).
- **Redirect code validation**: Only valid redirect codes (301, 302, 303, 307, 308) are accepted. Non-redirect codes like 304 are rejected at init time.
- **Compress BREACH warning**: Documentation warns about BREACH attacks when compressing HTTPS responses containing both user-controlled input and secrets.

### v1.3.0 Security Improvements

v1.3.0 includes hardening identified during a comprehensive 24-agent automated security review:

- **CORS**: `MirrorRequestHeaders` now validates tokens against RFC 7230 tchar charset with size/count limits (prevents header injection). `AllowCredentials` with wildcard subdomain origins now panics at init.
- **JWT**: Error classification distinguishes expired vs malformed vs invalid tokens. JWKS keys pre-fetched eagerly to eliminate first-request blocking.
- **BasicAuth**: Case-insensitive "Basic" scheme matching per RFC 7617.
- **KeyAuth**: `Vary` header on 401 responses for header-based lookups (cache correctness).
- **Recovery**: Exported sentinel errors (`ErrPanic`, `ErrBrokenPipe`, `ErrPanicContextCancelled`, `ErrPanicResponseCommitted`) for programmatic error matching.

### Previous Versions

Versions prior to 1.3.0 contain known security gaps in CORS header mirroring, JWT error disclosure, and BasicAuth scheme parsing. Versions prior to 1.2.0 contain known vulnerabilities in the HTTP response writer, cookie handling, header sanitization, and the `Detach()` streaming mechanism. We strongly recommend upgrading immediately.

## Reporting a Vulnerability

If you discover a security vulnerability in celeris, please report it responsibly.

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please email security@goceleris.dev with:

- A description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

We will acknowledge receipt within 48 hours and aim to provide a fix within 7 days for critical issues.

## Scope

This policy covers the core `github.com/goceleris/celeris` module and all in-tree middleware, including:

- HTTP protocol parsing (H1, H2)
- I/O engines (io_uring, epoll, std)
- Request routing, pre-routing middleware (`Server.Pre()`), and context handling
- The net/http bridge adapter
- Response header sanitization (CRLF, null bytes, cookie attributes)
- Connection lifecycle management (Detach, StreamWriter, pool safety)
- Body size enforcement (MaxRequestBodySize across H1, H2, bridge)
- Callback safety (OnExpectContinue, OnConnect, OnDisconnect)
- All in-tree middleware packages (`middleware/`)
- Sub-module middleware (`middleware/compress`, `middleware/metrics`, `middleware/otel`)

### Out of Scope

- The deprecated `github.com/goceleris/middlewares` module (archived, retracted)
- Third-party middleware not in this repository
- Application-level vulnerabilities in user code
