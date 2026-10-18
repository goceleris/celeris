#!/usr/bin/env bash
# Offline test of the build-info check of `mage tools` (celeris#890, live item
# 46). Magefiles carry `//go:build mage`, so no go test reaches them and the
# `mage -compile` step only proves they compile; this runs the real `mage tools`
# on stand-in h2spec binaries. Run from the repository root; the Lint job runs it.
#
# An h2spec on PATH that says "Version: 2.6.0" is the -ldflags stamp of any
# build, so `mage tools` must also read the module version the binary records
# and refuse a build of another version of h2spec's module (a bare @latest
# installs v2.2.1+incompatible with the stamp). The stand-ins are built from a
# throwaway module named like h2spec's, so nothing is downloaded and no
# network is needed:
#
#   stamped-other-version  tagged v1.5.0 (another version of the module), the
#                          stamp says 2.6.0: must be refused
#                          (and `mage tools` then fails offline at the install)
#   stamped-devel          untagged, so its version is "(devel)": must be kept
#
# Not covered here, because it needs the network to install h2spec: that a
# refused install is removed and a second run does not accept it (the full
# harness is in the PR description).
set -uo pipefail

repo=$PWD
dir=$(mktemp -d)
trap 'chmod -R u+w "$dir"; rm -rf "$dir"' EXIT
fail=0
n=0

# mage: a binary on PATH, or the one of .github/tools/go.mod (a `tool` there
# once the pins live in that module; built here, with the network the caller has).
if command -v mage >/dev/null 2>&1; then
  mage=$(command -v mage)
else
  mage=$dir/mage
  GOWORK=off go build -modfile="$repo/.github/tools/go.mod" -o "$mage" github.com/magefile/mage \
    || { echo "FAIL: no mage on PATH and building it from .github/tools failed"; exit 1; }
fi

# build NAME [TAG]: a stand-in h2spec in $dir/NAME/bin, stamped 2.6.0 as the
# Makefile's -ldflags do, built from a module named like h2spec's, tagged TAG.
build() {
  local name=$1 tag=${2:-} src=$dir/src-$1
  mkdir -p "$src" "$dir/$name/bin"
  printf 'module github.com/summerwind/h2spec\n\ngo 1.21\n' >"$src/go.mod"
  cat >"$src/main.go" <<'GO'
package main

import (
	"fmt"
	"os"
)

var VERSION, COMMIT string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("Version: %s (%s)\n", VERSION, COMMIT)
	}
}
GO
  (
    cd "$src" || exit 1
    git init -q . && git add . \
      && git -c user.name=t -c user.email=t@example.com commit -q -m stand-in || exit 1
    [ -z "$tag" ] || git tag "$tag"
    # Untagged, the build is of a checkout and records "(devel)"; tagged, the
    # go command stamps the tag from the repository.
    vcs=-buildvcs=auto
    [ -n "$tag" ] || vcs=-buildvcs=false
    go build "$vcs" -ldflags "-X main.VERSION=2.6.0 -X main.COMMIT=0000000" -o "$dir/$name/bin/h2spec" .
  ) || { echo "FAIL $name: could not build the stand-in"; fail=1; return 1; }
}

# tools NAME: `mage tools` with the stand-in first on PATH, offline, in the
# repository root; output in $dir/out, status in $status.
tools() {
  local name=$1 goroot
  goroot=$(go env GOROOT)
  status=0
  (
    cd "$repo" || exit 1
    PATH="$dir/$name/bin:$(dirname "$mage"):$goroot/bin:/usr/bin:/bin" \
    GOBIN="$dir/$name/gobin" GOPATH="$dir/$name/gopath" GOPROXY=off GOFLAGS='' \
    MAGEFILE_CACHE="$dir/$name/magecache" GOWORK=off \
      "$mage" tools
  ) >"$dir/out" 2>&1 || status=$?
}

# modversion NAME WANT: the recorded module version must be what the case needs,
# or the case would pass or fail for the wrong reason.
modversion() {
  local got
  got=$(go version -m "$dir/$1/bin/h2spec" | awk '$1 == "mod" { print $3 }')
  if [ "$got" != "$2" ]; then
    echo "FAIL $1: the stand-in records module version '$got', want '$2'"
    fail=1
    return 1
  fi
}

n=$((n + 1))
if build stamped-other-version v1.5.0 && modversion stamped-other-version v1.5.0; then
  tools stamped-other-version
  if grep -q 'already installed' "$dir/out" || ! grep -q 'but was built from github.com/summerwind/h2spec@v1.5.0' "$dir/out"; then
    echo "FAIL stamped-other-version: mage tools kept a 2.6.0-stamped build of v1.5.0:"
    cat "$dir/out"
    fail=1
  else
    echo "ok   stamped-other-version (refused)"
  fi
fi

n=$((n + 1))
if build stamped-devel && modversion stamped-devel '(devel)'; then
  tools stamped-devel
  if [ "$status" -ne 0 ] || ! grep -q 'already installed' "$dir/out"; then
    echo "FAIL stamped-devel: mage tools refused a build of a checkout (exit $status):"
    cat "$dir/out"
    fail=1
  else
    echo "ok   stamped-devel (kept)"
  fi
fi

echo "$n cases"
exit "$fail"
