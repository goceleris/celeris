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
  # The requirements files of the Python tools (check 4), where there are any.
  find "$repo/.github" -name 'requirements*.txt' -type f | while IFS= read -r f; do
    mkdir -p "$dir/$1/$(dirname "${f#"$repo"/}")"
    cp "$f" "$dir/$1/${f#"$repo"/}"
  done
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

# 4. Python tools: a version of their own in a workflow, and a requirements
# file that no Dependabot pip entry watches (celeris#890 item 93).
fixture pipx-spec
printf '      - run: pipx run --spec zizmor==1.30.0 zizmor .github/workflows\n' >>"$dir/pipx-spec/.github/workflows/ci.yml"
expect pipx-spec 1 'ci.yml'
fixture pip-install-pin
printf '      - run: python3 -m pip install zizmor==1.30.0\n' >>"$dir/pip-install-pin/.github/workflows/ci.yml"
expect pip-install-pin 1 'ci.yml'
fixture pip-install-extras
printf '      - run: pip3 install --quiet "zizmor[x]==1.30.0"\n' >>"$dir/pip-install-extras/.github/workflows/ci.yml"
expect pip-install-extras 1 'ci.yml'
fixture uvx-pin
printf '      - run: uvx --from zizmor==1.30.0 zizmor .\n' >>"$dir/uvx-pin/.github/workflows/ci.yml"
expect uvx-pin 1 'ci.yml'
fixture pip-install-continuation
printf '      - run: |\n          pipx run \\\n            --spec zizmor==1.30.0 zizmor .\n' >>"$dir/pip-install-continuation/.github/workflows/ci.yml"
expect pip-install-continuation 1 'ci.yml'
fixture pip-install-composite
printf '    - run: pipx install zizmor==1.30.0\n      shell: bash\n' >>"$dir/pip-install-composite/.github/actions/probe/action.yml"
expect pip-install-composite 1 'actions/probe/action.yml'
# Pin forms other than ==: uv's name@version, and range pins (review r1 of #1124, m4).
fixture uvx-at-pin
printf '      - run: uvx zizmor@1.30.0 .github/workflows\n' >>"$dir/uvx-at-pin/.github/workflows/ci.yml"
expect uvx-at-pin 1 'ci.yml'
fixture uv-tool-run-at-pin
printf '      - run: uv tool run zizmor@v1.30.0 .\n' >>"$dir/uv-tool-run-at-pin/.github/workflows/ci.yml"
expect uv-tool-run-at-pin 1 'ci.yml'
fixture pip-range-pin
printf '      - run: pip install "zizmor>=1.30,<1.31"\n' >>"$dir/pip-range-pin/.github/workflows/ci.yml"
expect pip-range-pin 1 'ci.yml'
fixture pip-compatible-pin
printf '      - run: pipx run --spec zizmor~=1.30 zizmor .\n' >>"$dir/pip-compatible-pin/.github/workflows/ci.yml"
expect pip-compatible-pin 1 'ci.yml'
fixture pip-upper-pin
printf '      - run: python3 -m pip install "zizmor<2"\n' >>"$dir/pip-upper-pin/.github/workflows/ci.yml"
expect pip-upper-pin 1 'ci.yml'
# An @ that is no version (a path, a mail address) and a plain redirection are fine.
fixture uvx-at-path
printf '      - run: uvx zizmor .github/workflows 2>&1 | tee log@host.txt\n' >>"$dir/uvx-at-path/.github/workflows/ci.yml"
expect uvx-at-path 0
fixture pip-redirect
printf '      - run: pip install -r .github/tools/requirements.txt >/dev/null\n' >>"$dir/pip-redirect/.github/workflows/ci.yml"
expect pip-redirect 0

# Reading the pins from a requirements file, and a comment, are fine.
fixture pip-install-requirements
printf '      - run: pip install -r .github/tools/requirements.txt\n' >>"$dir/pip-install-requirements/.github/workflows/ci.yml"
expect pip-install-requirements 0
fixture pip-install-comment
printf '      # was: pipx run --spec zizmor==1.30.0 zizmor .\n' >>"$dir/pip-install-comment/.github/workflows/ci.yml"
expect pip-install-comment 0
# An == that is no pin of a Python tool is fine.
fixture eq-in-condition
printf '      - run: if [ "$x" == "pip install" ]; then echo ok; fi\n' >>"$dir/eq-in-condition/.github/workflows/ci.yml"
expect eq-in-condition 0

# A requirements file in a directory no pip entry names, and the entry added
# in either spelling.
fixture requirements-unwatched
mkdir -p "$dir/requirements-unwatched/.github/ci-extra"
printf 'zizmor==1.30.0\n' >"$dir/requirements-unwatched/.github/ci-extra/requirements.txt"
expect requirements-unwatched 1 "no 'package-ecosystem: pip' entry"
fixture requirements-watched
mkdir -p "$dir/requirements-watched/.github/ci-extra"
printf 'zizmor==1.30.0\n' >"$dir/requirements-watched/.github/ci-extra/requirements.txt"
printf '  - package-ecosystem: pip\n    directory: /.github/ci-extra\n    schedule:\n      interval: daily\n' >>"$dir/requirements-watched/.github/dependabot.yml"
expect requirements-watched 0
fixture requirements-watched-list
mkdir -p "$dir/requirements-watched-list/.github/ci-extra"
printf 'zizmor==1.30.0\n' >"$dir/requirements-watched-list/.github/ci-extra/requirements.txt"
printf '  - package-ecosystem: pip\n    directories:\n      - /somewhere\n      - /.github/ci-extra\n    schedule:\n      interval: daily\n' >>"$dir/requirements-watched-list/.github/dependabot.yml"
expect requirements-watched-list 0
# An entry of another ecosystem for the same directory does not watch pip.
fixture requirements-wrong-ecosystem
mkdir -p "$dir/requirements-wrong-ecosystem/.github/ci-extra"
printf 'zizmor==1.30.0\n' >"$dir/requirements-wrong-ecosystem/.github/ci-extra/requirements.txt"
printf '  - package-ecosystem: gomod\n    directory: /.github/ci-extra\n    schedule:\n      interval: daily\n' >>"$dir/requirements-wrong-ecosystem/.github/dependabot.yml"
expect requirements-wrong-ecosystem 1 "no 'package-ecosystem: pip' entry"

echo "$n cases"
exit "$fail"
