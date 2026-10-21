// Versions of the Go tools CI runs, kept out of the celeris module so they
// add nothing to its dependency graph. Each is a `tool` directive, pinned by
// go.sum and bumped by Dependabot (.github/dependabot.yml): govulncheck, mage
// and actionlint, which the Lint, Vulnerability Check and release workflows run,
// and benchstat. A workflow runs one with
// `GOWORK=off go tool -modfile=$GITHUB_WORKSPACE/.github/tools/go.mod <name>`.
// A new `tool` needs its module in the allow list of dependabot.yml's
// /.github/tools entry; the Lint step "Tool pins agree" fails without it.
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

go 1.27.3

tool (
	github.com/magefile/mage
	github.com/rhysd/actionlint/cmd/actionlint
	golang.org/x/perf/cmd/benchstat
	golang.org/x/vuln/cmd/govulncheck
)

require golang.org/x/tools v0.52.0

require (
	github.com/aclements/go-moremath v0.0.0-20241023150245-c8bbc672ef66 // indirect
	github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/magefile/mage v1.17.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.21 // indirect
	github.com/mattn/go-shellwords v1.0.12 // indirect
	github.com/rhysd/actionlint v1.7.12 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.3 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/perf v0.0.0-20261009192801-be2c69fb417e // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/telemetry v0.0.0-20261012234344-3c2f9838340e // indirect
	golang.org/x/vuln v1.9.0 // indirect
)
