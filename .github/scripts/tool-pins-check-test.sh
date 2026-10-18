#!/usr/bin/env bash
# Self-test of tool-pins-check.sh on fixtures built from this repository's own
# files with one defect each (celeris#890). Run from the repository root; the
# Lint job runs it, so a change that makes the check pass on a broken input is
# a red Lint. Each fixture is a copy of the real files, so the passing case is
# the real configuration and every failing case differs from it by one edit.
#
#   TOOL_PINS_CHECK=path   the check to test (default .github/scripts/tool-pins-check.sh)
set -uo pipefail

repo=$PWD
check=${TOOL_PINS_CHECK:-$repo/.github/scripts/tool-pins-check.sh}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
fail=0
n=0

# fixture NAME: a copy of the files the check reads, in $dir/NAME.
fixture() {
  mkdir -p "$dir/$1/.github/tools" "$dir/$1/.github/workflows"
  cp "$repo/.github/tools/go.mod" "$repo/.github/tools/go.sum" "$dir/$1/.github/tools/"
  cp "$repo/.github/dependabot.yml" "$dir/$1/.github/"
  cp "$repo"/.github/workflows/*.yml "$dir/$1/.github/workflows/"
  mkdir -p "$dir/$1/.github/actions/probe"
  printf 'name: probe\nruns:\n  using: composite\n  steps:\n    - run: echo ok\n      shell: bash\n' >"$dir/$1/.github/actions/probe/action.yml"
  cp "$repo/CONTRIBUTING.md" "$dir/$1/"
}

# expect NAME WANT_STATUS WANT_STDERR_SUBSTRING: runs the check on $dir/NAME.
expect() {
  local name=$1 want=$2 sub=${3:-} status=0
  "$check" "$dir/$name" >"$dir/out" 2>"$dir/err" || status=$?
  n=$((n + 1))
  if [ "$status" -ne "$want" ]; then
    echo "FAIL $name: exit $status, want $want"
    cat "$dir/err"
    fail=1
  elif [ -n "$sub" ] && ! grep -qF -e "$sub" "$dir/err"; then
    echo "FAIL $name: stderr does not say '$sub':"
    cat "$dir/err"
    fail=1
  else
    echo "ok   $name"
  fi
}

# sedi EXPR FILE: in-place sed that must change the file (a control whose
# edit does not apply would pass for the wrong reason).
sedi() {
  cp "$2" "$2.orig"
  sed -E "$1" "$2.orig" >"$2"
  if cmp -s "$2" "$2.orig"; then echo "FAIL fixture: '$1' did not change $2"; fail=1; fi
  rm "$2.orig"
}

fixture good
expect good 0

# 1. Dependabot allow list.
fixture allow-package-path
sedi 's#dependency-name: golang.org/x/vuln$#dependency-name: golang.org/x/vuln/cmd/govulncheck#' "$dir/allow-package-path/.github/dependabot.yml"
expect allow-package-path 1 'golang.org/x/vuln/cmd/govulncheck'

fixture tool-without-entry
sedi 's#dependency-name: github.com/magefile/mage$#dependency-name: github.com/magefile/mage-gone#' "$dir/tool-without-entry/.github/dependabot.yml"
expect tool-without-entry 1 'in the tools but not allowed: github.com/magefile/mage'

# A tool directive added without its entry (stringer, from x/tools), and an
# entry whose tool directive is gone.
fixture tool-added-without-entry
awk '{ print } /rhysd\/actionlint\/cmd\/actionlint$/ { print "\tgolang.org/x/tools/cmd/stringer" }' "$dir/tool-added-without-entry/.github/tools/go.mod" >"$dir/go.mod.tmp"
mv "$dir/go.mod.tmp" "$dir/tool-added-without-entry/.github/tools/go.mod"
expect tool-added-without-entry 1 'in the tools but not allowed: golang.org/x/tools'

fixture entry-without-tool
grep -v 'rhysd/actionlint/cmd/actionlint$' "$dir/entry-without-tool/.github/tools/go.mod" >"$dir/go.mod.tmp"
mv "$dir/go.mod.tmp" "$dir/entry-without-tool/.github/tools/go.mod"
expect entry-without-tool 1 'allowed but not a tool module: github.com/rhysd/actionlint'

# A dependency-name outside the /.github/tools entry does not count.
fixture entry-in-wrong-block
sedi 's#^([[:space:]]*)- dependency-name: github.com/rhysd/actionlint$#\1- dependency-name: github.com/rhysd/actionlint-not#' "$dir/entry-in-wrong-block/.github/dependabot.yml"
printf '  - package-ecosystem: gomod\n    directory: /elsewhere\n    allow:\n      - dependency-name: github.com/rhysd/actionlint\n' >>"$dir/entry-in-wrong-block/.github/dependabot.yml"
expect entry-in-wrong-block 1 'in the tools but not allowed: github.com/rhysd/actionlint'

# The keys of an entry in another order (allow before directory) do not matter.
fixture allow-before-directory
awk '
  /^    directory: \/\.github\/tools$/ { held = $0; next }
  /^    schedule:$/ && held != "" { print held; held = "" }
  { print }
' "$dir/allow-before-directory/.github/dependabot.yml" >"$dir/dep.tmp"
mv "$dir/dep.tmp" "$dir/allow-before-directory/.github/dependabot.yml"
if ! grep -B40 -m1 '^    directory: /.github/tools$' "$dir/allow-before-directory/.github/dependabot.yml" | grep -q 'dependency-name: github.com/rhysd/actionlint'; then
  echo "FAIL fixture: allow-before-directory did not move the directory line"; fail=1
fi
expect allow-before-directory 0

# 2. golangci-lint versions.
fixture doc-drift
sedi 's#(golangci-lint\].*) v2\.[0-9]+,#\1 v2.99,#' "$dir/doc-drift/CONTRIBUTING.md"
expect doc-drift 1 "names golangci-lint 'v2.99'"

fixture ci-two-versions
awk '/version: v2\.[0-9]+$/ && !done { sub(/v2\.[0-9]+/, "v2.98"); done=1 } { print }' "$dir/ci-two-versions/.github/workflows/ci.yml" >"$dir/ci.tmp"
mv "$dir/ci.tmp" "$dir/ci-two-versions/.github/workflows/ci.yml"
expect ci-two-versions 1 'must pin exactly one'

# 3. go install @version in a workflow.
fixture go-install
printf '      - run: go install github.com/magefile/mage@v1.17.2\n' >>"$dir/go-install/.github/workflows/release.yml"
expect go-install 1 'release.yml'
fixture go-install-latest
printf '      - run: go install golang.org/x/tools/cmd/stringer@latest\n' >>"$dir/go-install-latest/.github/workflows/ci.yml"
expect go-install-latest 1 'ci.yml'
# A comment that mentions go install is not an install.
fixture go-install-comment
printf '      # was: go install github.com/magefile/mage@v1.17.2\n' >>"$dir/go-install-comment/.github/workflows/ci.yml"
expect go-install-comment 0

# The same defect in forms the first fixtures did not have: the package on a
# continuation line, after an earlier # on the line, in a .yaml file, in a
# composite action, and run with go run.
fixture go-install-continuation
printf '      - run: |\n          go install \\\n            github.com/magefile/mage@v1.17.2\n' >>"$dir/go-install-continuation/.github/workflows/ci.yml"
expect go-install-continuation 1 'ci.yml'
fixture go-install-after-hash
printf '      - run: echo "# tool" && go install github.com/magefile/mage@v1.17.2\n' >>"$dir/go-install-after-hash/.github/workflows/ci.yml"
expect go-install-after-hash 1 'ci.yml'
fixture go-install-yaml
mkdir -p "$dir/go-install-yaml/.github/workflows"
printf 'name: x\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go install github.com/magefile/mage@v1.17.2\n' >"$dir/go-install-yaml/.github/workflows/extra.yaml"
expect go-install-yaml 1 'extra.yaml'
fixture go-install-action
printf '    - run: go install github.com/magefile/mage@v1.17.2\n      shell: bash\n' >>"$dir/go-install-action/.github/actions/probe/action.yml"
expect go-install-action 1 'actions/probe/action.yml'
fixture go-run
printf '      - run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...\n' >>"$dir/go-run/.github/workflows/ci.yml"
expect go-run 1 'ci.yml'
# go run without a pinned package, and an @ that is not a version, are fine.
fixture go-run-local
printf '      - run: go run ./cmd/tool -mail me@example.com\n' >>"$dir/go-run-local/.github/workflows/ci.yml"
expect go-run-local 0

echo "$n cases"
exit "$fail"
