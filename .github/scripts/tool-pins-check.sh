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
#   3. No workflow (.github/workflows/*.y{,a}ml) or composite action
#      (.github/actions/**/*.y{,a}ml) installs or runs a tool with
#      `go install pkg@version` or `go run pkg@version` (@latest is the same
#      defect): tools are `tool` directives of .github/tools/go.mod, run with
#      `go tool -modfile`, so Dependabot bumps them and go.sum pins them. A
#      backslash continuation is joined to its line first; a line that starts
#      with # is a comment and is skipped; a # later in a line is not (it can
#      sit in an echo string), so a trailing comment that quotes such a command
#      is flagged too. A YAML folded scalar (run: >) is not joined.
#
# Prints one line per failed check and exits 1 if any failed. The Lint job
# runs it, and tool-pins-check-test.sh runs it on broken fixtures.
set -uo pipefail

root=${1:-.}
fail=0
bad() { echo "tool-pins-check: $*" >&2; fail=1; }

# 1. tool modules against the Dependabot allow list.
tools=$(cd "$root/.github/tools" && GOWORK=off go list -f '{{.Module.Path}}' tool | sed '/^$/d' | sort -u) \
  || { bad "go list tool failed in $root/.github/tools"; tools=; }
# The Python is in a function so that bash 3.2 (macOS) parses the quotes in it.
allow_names() {
  python3 - "$1" <<'PY'
import re, sys
# The dependency-name lines of the updates entry whose directory is
# /.github/tools. An entry runs from its "- package-ecosystem:" line to the
# next one; it is judged when it ends, so the order of its keys does not matter.
names = []
entry_names, entry_dir = [], None
def end_entry():
    if entry_dir == "/.github/tools":
        names.extend(entry_names)
for line in open(sys.argv[1]).read().splitlines():
    if re.match(r'^\s*-\s*package-ecosystem:', line):
        end_entry()
        entry_names, entry_dir = [], None
    m = re.match(r'^\s*directory:\s*["\']?([^\s"\'#]+)["\']?\s*(#.*)?$', line)
    if m:
        entry_dir = m.group(1)
    m = re.match(r'^\s*-\s*dependency-name:\s*["\']?([^\s"\'#]+)', line)
    if m:
        entry_names.append(m.group(1))
end_entry()
print("\n".join(sorted(set(names))))
PY
}
allow=$(allow_names "$root/.github/dependabot.yml")
if [ -z "$tools" ]; then
  [ "$fail" = 1 ] || bad ".github/tools/go.mod has no tool directive"
elif [ "$tools" != "$allow" ]; then
  bad ".github/dependabot.yml's /.github/tools entry must name exactly the modules of .github/tools/go.mod's tools (the module path, not a package path)"
  echo "  tool modules: $(echo "$tools" | paste -sd' ' -)" >&2
  echo "  allow names:  $(echo "$allow" | paste -sd' ' -)" >&2
  echo "  in the tools but not allowed: $(comm -23 <(echo "$tools") <(echo "$allow") | paste -sd' ' -)" >&2
  echo "  allowed but not a tool module: $(comm -13 <(echo "$tools") <(echo "$allow") | paste -sd' ' -)" >&2
fi

# 2. golangci-lint: CONTRIBUTING.md against ci.yml.
ci=$(grep -E '^[[:space:]]+version: v2\.[0-9]+[[:space:]]*$' "$root/.github/workflows/ci.yml" | awk '{print $2}' | sort -u)
doc=$(grep -E 'golangci-lint' "$root/CONTRIBUTING.md" | grep -oE 'v2\.[0-9]+' | sort -u)
if [ -z "$ci" ] || [ "$(echo "$ci" | wc -l)" -ne 1 ]; then
  bad "ci.yml's golangci-lint-action steps must pin exactly one 'version: v2.N' (found: $(echo "$ci" | paste -sd' ' -))"
elif [ "$doc" != "$ci" ]; then
  bad "CONTRIBUTING.md names golangci-lint '$(echo "$doc" | paste -sd' ' -)', ci.yml pins '$ci'"
fi

# 3. no `go install pkg@version` or `go run pkg@version` in a workflow or action.
install_hits() {
  python3 - "$1" <<'PY'
import glob, os, re, sys
root = sys.argv[1]
pats = ['.github/workflows/*.yml', '.github/workflows/*.yaml',
        '.github/actions/**/*.yml', '.github/actions/**/*.yaml']
# `go install` / `go run`, then (flags) a package path with a slash, then @.
pat = re.compile(r'\bgo\s+(?:install|run)\b.*?\s[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]*@\S')
files = sorted({f for p in pats for f in glob.glob(os.path.join(root, p), recursive=True)})
for f in files:
    lines = open(f).read().splitlines()
    i = 0
    while i < len(lines):
        start, text = i, lines[i]
        while text.rstrip().endswith('\\') and i + 1 < len(lines):
            i += 1
            text = text.rstrip()[:-1] + ' ' + lines[i].strip()
        i += 1
        if text.lstrip().startswith('#'):
            continue
        if pat.search(text):
            print("%s:%d:%s" % (os.path.relpath(f, root), start + 1, lines[start].strip()))
PY
}
hits=$(install_hits "$root")
if [ -n "$hits" ]; then
  bad "a workflow or action installs or runs a tool with go install/go run pkg@version; add it to .github/tools/go.mod (go get -tool) and run it with go tool -modfile:"
  echo "  ${hits//$'\n'/$'\n'  }" >&2
fi

[ "$fail" = 0 ] && echo "tool-pins-check: ok (tools: $(echo "$tools" | paste -sd' ' -); golangci-lint $ci)"
exit "$fail"
