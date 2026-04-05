# Security Policy

## Supported Versions

| Version        | Supported          |
|----------------|--------------------|
| >= 1.3.0       | Yes                |
| < 1.3.0        | No                 |

Only the latest minor release receives security patches. Upgrade to the latest version to ensure you have all fixes.

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
- Sub-module middleware (`middleware/metrics`, `middleware/otel`)

### Out of Scope

- The deprecated `github.com/goceleris/middlewares` module (archived, retracted)
- Third-party middleware not in this repository
- Application-level vulnerabilities in user code
