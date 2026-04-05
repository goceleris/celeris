# Security Policy

## Supported Versions

| Version        | Supported          |
|----------------|--------------------|
| >= 1.2.0       | Yes                |
| < 1.2.0        | No                 |

Only the latest minor release receives security patches. Upgrade to the latest version to ensure you have all fixes.

Versions prior to 1.2.0 contain known vulnerabilities in the HTTP response writer, cookie handling, header sanitization, and the `Detach()` streaming mechanism that were identified and fixed during the v1.2.0 security review process. We strongly recommend upgrading immediately.

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

This policy covers the core `github.com/goceleris/celeris` module, including:

- HTTP protocol parsing (H1, H2)
- I/O engines (io_uring, epoll, std)
- Request routing and context handling
- The net/http bridge adapter
- Response header sanitization (CRLF, null bytes, cookie attributes)
- Connection lifecycle management (Detach, StreamWriter, pool safety)
- Body size enforcement (MaxRequestBodySize across H1, H2, bridge)
- Callback safety (OnExpectContinue, OnConnect, OnDisconnect)

The in-tree middleware packages (`middleware/`) are covered by this same policy.
