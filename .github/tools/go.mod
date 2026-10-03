// Versions of the Go tools CI and the repo's scripts run, kept out of the
// celeris module so they add nothing to its dependency graph. Each is a
// `tool` directive, pinned by go.sum and bumped by Dependabot
// (.github/dependabot.yml); a workflow runs one with
// `go tool -modfile=$GITHUB_WORKSPACE/.github/tools/go.mod <name>`, and
// .github/scripts/bench-ab.sh runs benchstat the same way (celeris#838).
// This module is never imported or released (celeris#827).
module celeris-ci-tools

go 1.27.0

tool (
	golang.org/x/perf/cmd/benchstat
	golang.org/x/vuln/cmd/govulncheck
)

require (
	github.com/aclements/go-moremath v0.0.0-20210112150236-f10218a38794 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/perf v0.0.0-20260929162123-406019bb8b68 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260908163034-4bcc4b2ee518 // indirect
	golang.org/x/tools v0.50.0 // indirect
	golang.org/x/vuln v1.8.0 // indirect
)
