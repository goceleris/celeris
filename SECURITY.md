# Security Policy

## Supported Versions

| Version        | Supported          |
|----------------|--------------------|
| >= 1.1.0       | Yes                |
| < 1.1.0        | No                 |

Only the latest minor release receives security patches. Upgrade to the latest version to ensure you have all fixes.

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
