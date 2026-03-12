# Contributing to Celeris

Thank you for your interest in contributing to celeris!

## Getting Started

1. Fork the repository
2. Clone your fork: `git clone https://github.com/<you>/celeris.git`
3. Install dependencies: `go mod download`
4. Run checks: `mage check`

## Development

### Prerequisites

- Go 1.26+
- [Mage](https://magefile.org) build tool: `go install github.com/magefile/mage@latest`
- Linux (for io_uring/epoll engine tests) or macOS (std engine only)

### Build & Test

```bash
mage lint      # Run linters
mage test      # Run tests with race detection
mage build     # Verify compilation
mage check     # Run all checks
```

### Linux Testing from macOS

Use the Multipass VM targets:

```bash
mage testlinux   # Run tests on a Linux VM
mage speclinux   # Run h2spec on a Linux VM
```

## Pull Requests

- Keep PRs focused on a single change
- Include tests for new functionality
- Run `mage check` before submitting
- Follow existing code style (enforced by golangci-lint)
- Write clear commit messages

## Code Style

- Follow standard Go conventions
- No unnecessary comments — code should be self-documenting
- Use `mage lint` to verify

## Reporting Issues

Use GitHub Issues with the provided templates for bug reports and feature requests.
