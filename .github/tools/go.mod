// Versions of the Go tools CI runs, kept out of the celeris module so they
// add nothing to its dependency graph. Each is a `tool` directive, pinned by
// go.sum and bumped by Dependabot (.github/dependabot.yml); a workflow runs
// one with `go tool -modfile=$GITHUB_WORKSPACE/.github/tools/go.mod <name>`.
// apidump, which keeps api/ in step with the exported API (celeris#443), is a
// package of this module itself; CI runs it with
// `go -C .github/tools run ./apidump`. It imports golang.org/x/tools at the
// version the pinned govulncheck release requires, so the two move together.
// This module is never imported or released (celeris#827).
//
// benchstat is here for .github/scripts/bench-ab.sh, which runs it the same
// way (celeris#838). golang.org/x/perf has no tagged release, and Dependabot's
// version updates propose tagged versions only, so that pin moves by hand:
// `GOWORK=off go -C .github/tools get golang.org/x/perf@latest`.
module celeris-ci-tools

go 1.27.0

tool (
	golang.org/x/perf/cmd/benchstat
	golang.org/x/vuln/cmd/govulncheck
)

require golang.org/x/tools v0.50.0

require (
	github.com/aclements/go-moremath v0.0.0-20210112150236-f10218a38794 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/perf v0.0.0-20260929162123-406019bb8b68 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260908163034-4bcc4b2ee518 // indirect
	golang.org/x/vuln v1.8.0 // indirect
)
