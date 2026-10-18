#!/usr/bin/env bash
# Checks that the places which name a pinned CI tool agree (celeris#890).
# Run from the repository root, or pass it:
#
#   tool-pins-check.sh [ROOT]
#
#   1. Every module that provides a `tool` of .github/tools/go.mod has a
#      `dependency-name:` entry in the /.github/tools block of
#      .github/dependabot.yml, and every entry names such a module. A tool
#      without an entry, or an entry that names a package path (say
#      golang.org/x/vuln/cmd/govulncheck) instead of the module path, stops
#      Dependabot proposing bumps for that tool without any error (verified
#      with Dependabot CLI v1.93.0).
#   2. The golangci-lint version CONTRIBUTING.md quotes is the one version the
#      golangci-lint-action steps of ci.yml pin (they must pin one).
#   3. No workflow installs a tool with `go install ...@version`: tools are
#      `tool` directives of .github/tools/go.mod, run with `go tool -modfile`,
#      so Dependabot bumps them and go.sum pins them.
#
# Prints one line per failed check and exits 1 if any failed. The Lint job
# runs it, and tool-pins-check-test.sh runs it on broken fixtures.
set -uo pipefail

root=${1:-.}
fail=0
bad() { echo "tool-pins-check: $*" >&2; fail=1; }

# 1. tool modules against the Dependabot allow list.
tools=$(cd "$root/.github/tools" && GOWORK=off go list -f '{{.Module.Path}}' tool | sort -u) \
  || { bad "go list tool failed in $root/.github/tools"; tools=; }
# The Python is in a function so that bash 3.2 (macOS) parses the quotes in it.
allow_names() {
  python3 - "$1" <<'PY'
import re, sys
# The dependency-name lines inside the updates entry whose directory is
# /.github/tools (an entry runs from its "- package-ecosystem:" line to the next).
inside = False
names = []
for line in open(sys.argv[1]).read().splitlines():
    if re.match(r'^\s*-\s*package-ecosystem:', line):
        inside = False
    if re.match(r'^\s*directory:\s*["\']?/\.github/tools["\']?\s*(#.*)?$', line):
        inside = True
    m = re.match(r'^\s*-\s*dependency-name:\s*["\']?([^\s"\'#]+)', line)
    if inside and m:
        names.append(m.group(1))
print("\n".join(sorted(set(names))))
PY
}
allow=$(allow_names "$root/.github/dependabot.yml")
if [ -z "$tools" ]; then
  [ "$fail" = 1 ] || bad ".github/tools/go.mod has no tool directive"
elif [ "$tools" != "$allow" ]; then
  bad ".github/dependabot.yml's /.github/tools entry must name exactly the modules of .github/tools/go.mod's tools (the module path, not a package path)"
  echo "  tool modules: $(echo $tools)" >&2
  echo "  allow names:  $(echo $allow)" >&2
  echo "  in the tools but not allowed: $(comm -23 <(echo "$tools") <(echo "$allow") | tr '\n' ' ')" >&2
  echo "  allowed but not a tool module: $(comm -13 <(echo "$tools") <(echo "$allow") | tr '\n' ' ')" >&2
fi

# 2. golangci-lint: CONTRIBUTING.md against ci.yml.
ci=$(grep -E '^[[:space:]]+version: v2\.[0-9]+[[:space:]]*$' "$root/.github/workflows/ci.yml" | awk '{print $2}' | sort -u)
doc=$(grep -E 'golangci-lint' "$root/CONTRIBUTING.md" | grep -oE 'v2\.[0-9]+' | sort -u)
if [ -z "$ci" ] || [ "$(echo "$ci" | wc -l)" -ne 1 ]; then
  bad "ci.yml's golangci-lint-action steps must pin exactly one 'version: v2.N' (found: $(echo $ci))"
elif [ "$doc" != "$ci" ]; then
  bad "CONTRIBUTING.md names golangci-lint '$(echo $doc)', ci.yml pins '$ci'"
fi

# 3. no `go install ...@version` in a workflow.
hits=$(grep -nE '^[^#]*go install\b.*@' "$root"/.github/workflows/*.yml)
if [ -n "$hits" ]; then
  bad "a workflow installs a tool with go install @version; add it to .github/tools/go.mod (go get -tool) and run it with go tool -modfile:"
  echo "$hits" | sed 's/^/  /' >&2
fi

[ "$fail" = 0 ] && echo "tool-pins-check: ok (tools: $(echo $tools); golangci-lint $ci)"
exit "$fail"
