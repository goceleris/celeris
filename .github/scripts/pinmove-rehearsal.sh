#!/usr/bin/env bash
# Dry run of the release's sub-module pin move (celeris#619). Nothing is
# published: the "remote" is a bare repository in a temp dir, git is pointed at
# it with url.insteadOf, and GOPRIVATE makes the go command fetch every
# github.com/goceleris/celeris module from it over VCS instead of the public
# proxy. Third-party dependencies still come from the public module proxy.
#
# Run from the repository root, on a clean checkout with full history and tags:
#
#   .github/scripts/pinmove-rehearsal.sh [VERSION]
#
#   VERSION                 must match the release gate's semver (vX.Y.Z or
#                           vX.Y.Z-(alpha|beta|rc).N). Default: derived at run
#                           time by `mage RehearsalVersion` from server.go's
#                           Version and the release tags, so it is always newer
#                           than the last release (v1.6.0-rc.99 while v1.6.0 is
#                           unreleased, v1.6.1-rc.99 once it ships)
#   PINMOVE_KEEP=1          keep the temp dir (default: removed on exit)
#   PINMOVE_SKIP_PREP=1     negative control: skip PrepRelease. The script must
#                           then exit non-zero (the pins still name the last
#                           released celeris, which is the defect of #619)
#
# What it does, in the order a release does it:
#   0  drift check: every published module is stamped by PrepRelease, tagged by
#      release.yml and requested from the proxy by release.yml
#   1  a version the gate refuses is refused
#   2  PrepRelease; CheckRelease fails on the README placeholder, then passes
#      in release mode once the placeholder is replaced
#   3  commit the prep PR to the "remote"; each sub-module pinned at the prep
#      commit's pseudo-version must NOT resolve yet (the probatorium case)
#   9  control: sub-module tags without a root tag must not resolve
#   4  root tag; then release.yml's "Create sub-module tags" step, extracted
#      from the workflow file and run unchanged
#   5  an external consumer module (no replace directive, outside the tree)
#      requires each sub-module in turn: celeris must resolve to VERSION
#   6  release.yml's "Verify module availability" query, per module
#   7  the same consumers again with GOPROXY=file:// over the module cache
#      step 5 filled, a fresh GOMODCACHE, GOSUMDB=off; the celeris go.sum
#      lines must equal step 5's
#   8  control: the same tags on a tree WITHOUT the pin move; the assertion of
#      step 5 must go red (celeris resolves to the last release)
#   10 retraction probe: a newer sub-module tag retracting a bad one
#
# What it can touch: git and the go command read and write only under a
# temp dir (GIT_CONFIG_GLOBAL, GOPATH, GOMODCACHE, GOCACHE, MAGEFILE_CACHE and
# GOENV=off keep the user's configuration out). Every remote it pushes to is a
# bare repository in that temp dir; the checkout it runs in is only read. git
# may use no transport but file (GIT_ALLOW_PROTOCOL), and a push to any
# github.com URL is rewritten to a dead path. The only network use is the go
# command reading third-party modules from the public module proxy; celeris
# modules are GOPRIVATE, so no rehearsal version is ever asked of the proxy or
# the checksum database.
set -Eeuo pipefail
trap 'echo "pinmove-rehearsal: stopped at line $LINENO (status $?)" >&2' ERR

ROOT=$PWD
MODPATH=github.com/goceleris/celeris
GATE_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)\.[0-9]+)?$'
T=$(mktemp -d)
cleanup() {
  local rc=$?
  trap - EXIT
  if [ -n "${PINMOVE_KEEP:-}" ]; then
    echo "temp dir kept at $T" >&2
  elif [ -n "${T:-}" ] && [ -d "$T" ]; then
    chmod -R u+w "$T" 2>/dev/null || true # the module cache is read-only without -modcacherw
    rm -rf "$T"
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
pass=0
fail=0
ok()   { pass=$((pass + 1)); echo "  PASS  $*"; }
bad()  { fail=$((fail + 1)); echo "  FAIL  $*"; }
step() { echo; echo "== $*"; }
# expect_ok DESC CMD...: the command must succeed.   expect_red DESC CMD...: it must fail.
expect_ok()  { local d=$1; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }
expect_red() { local d=$1; shift; if "$@"; then bad "$d (it passed; it must go red)"; else ok "$d (red, as it must be)"; fi; }

command -v go >/dev/null || { echo "go not found" >&2; exit 2; }
[ -f go.mod ] && [ -d magefiles ] || { echo "run from the celeris repository root" >&2; exit 2; }
[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "tracked files are modified; commit or stash first" >&2; exit 2; }
LAST=$(git tag --list 'v[0-9]*' --sort=-v:refname | { grep -v -- '-' || true; } | head -n1)
[ -n "$LAST" ] || { echo "no release tags here: fetch with full history and tags" >&2; exit 2; }

# The user's own module cache is never the rehearsal's (forget_celeris deletes in it).
USER_MODCACHE=$(GOENV=off go env GOMODCACHE)
if [ -n "${PINMOVE_MODCACHE:-}" ] && [ "$PINMOVE_MODCACHE" = "$USER_MODCACHE" ]; then
  echo "PINMOVE_MODCACHE must not be the default module cache ($USER_MODCACHE)" >&2; exit 2
fi
GOVER=$(sed -n 's/^go //p' go.mod | head -n1)
[ -n "$GOVER" ] || { echo "no go directive in go.mod" >&2; exit 2; }

# Everything the go command and git read or write lives under $T.
export GIT_CONFIG_GLOBAL=$T/gitconfig GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0
export GIT_ALLOW_PROTOCOL=file
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_SSH GIT_SSH_COMMAND
export GOPATH=$T/gopath GOCACHE=${PINMOVE_GOCACHE:-$T/gocache} GOFLAGS=-modcacherw GOTOOLCHAIN=local GOWORK=off
export GOMODCACHE=${PINMOVE_MODCACHE:-$T/modcache}
export GOENV=off MAGEFILE_CACHE=$T/magecache
export GOPRIVATE=$MODPATH
unset GOPROXY GONOSUMDB GONOPROXY GONOSUMCHECK GOINSECURE GOVCS
# base_config rewrites the whole git config, so exactly one insteadOf is live.
base_config() {
  : >"$GIT_CONFIG_GLOBAL"
  git config --global safe.directory '*'
  git config --global user.name pinmove-rehearsal
  git config --global user.email pinmove-rehearsal@invalid
  git config --global init.defaultBranch main
  git config --global protocol.file.allow always
  git config --global push.followTags false
  # Belt and braces: a push to GitHub, by any spelling of its URL, goes nowhere.
  local u
  for u in https://github.com/ git@github.com: ssh://git@github.com/ git://github.com/; do
    git config --global --add url."file:///nonexistent-pinmove-push-blocked/".pushInsteadOf "$u"
  done
}
base_config

SEED=$T/seed.git
git clone -q --bare --no-local "$ROOT" "$SEED"
git --git-dir="$SEED" update-ref refs/heads/main "$(git rev-parse HEAD)"

# redirect NAME: point https://github.com/goceleris/celeris at $T/NAME.git
# (and only there).
redirect() {
  base_config
  git config --global --add url."file://$T/$1.git".insteadOf "https://$MODPATH"
}
# fresh NAME: a new bare "remote" seeded from the checkout, and a clone of it.
fresh() {
  rm -rf "${T:?}/$1.git" "${T:?}/$1"
  cp -R "$SEED" "$T/$1.git"
  redirect "$1"
  git clone -q "$T/$1.git" "$T/$1"
}
# forget_celeris: drop cached VCS state and celeris downloads so the next
# lookup asks the (rewritten) remote again; third-party downloads stay.
forget_celeris() {
  rm -rf "$GOMODCACHE/cache/vcs" "$GOMODCACHE/cache/download/github.com/goceleris" "$GOMODCACHE/github.com/goceleris"
}
mage() { (cd "$1" && shift && go tool -modfile="$PWD/.github/tools/go.mod" mage "$@"); }
# local_origin DIR: DIR's push and fetch URL of origin are paths under the temp dir.
local_origin() {
  local d=$1 u
  for u in "$(git -C "$d" remote get-url origin)" "$(git -C "$d" remote get-url --push origin)"; do
    case "$u" in "$T"/*) ;; *) echo "REFUSING: $d's origin is $u, not under $T" >&2; return 1;; esac
  done
}

VERSION=${1:-}
if [ -z "$VERSION" ]; then
  VERSION=$(mage "$ROOT" RehearsalVersion) || { echo "could not derive the default version (mage RehearsalVersion failed)" >&2; exit 2; }
  DERIVED=" (derived from server.go and the release tags)"
fi
if ! [[ $VERSION =~ $GATE_RE ]]; then
  echo "VERSION=\"$VERSION\": want vX.Y.Z or vX.Y.Z-(alpha|beta|rc).N, which is what the release gate accepts" >&2; exit 2
fi
echo "rehearsal of $VERSION${DERIVED:-} on $(git rev-parse --short HEAD); last release $LAST; temp dir $T"
echo "go: $(go version)"

# consumer DIR SUBMODULE REF: a module outside the tree that requires one
# sub-module at REF. A dependency's replace directive is ignored here, so this
# is what a user of the released module sees.
consumer() {
  local dir=$1 sub=$2 ref=$3
  rm -rf "$dir"; mkdir -p "$dir"
  printf 'module example.com/consumer\n\ngo %s\n' "$GOVER" >"$dir/go.mod"
  printf 'package main\n\nimport _ "%s/%s"\n\nfunc main() {}\n' "$MODPATH" "$sub" >"$dir/main.go"
  (cd "$dir" && export GOFLAGS="-modcacherw -mod=mod" && go get "$MODPATH/$sub@$ref" 2>"$dir/get.err" && go build -o /dev/null . 2>>"$dir/get.err")
}
resolved_root() { (cd "$1" && GOFLAGS="-modcacherw -mod=mod" go list -m -f '{{.Version}}' $MODPATH 2>/dev/null); }
# resolves_to DIR WANT: celeris in the consumer's build list is exactly WANT.
resolves_to() { local got; got=$(resolved_root "$1"); [ "$got" = "$2" ] || { echo "      celeris resolved to '${got}', want $2"; return 1; }; }
celeris_sums() { { grep "^$MODPATH v" "$1/go.sum" 2>/dev/null || true; } | sort; }
# first_lines FILE N: the first N lines of an error file, indented, minus go's download chatter.
first_lines() { { grep -v '^go: downloading' "$1" || true; } | sed -n "1,${2}p" | sed 's/^/      /'; }

##########################################################################
step "0 drift check: published modules vs PrepRelease vs release.yml"
published=$(find . -name go.mod -not -path './.git/*' -not -path './test/*' -not -path './.github/*' ! -path ./go.mod | sed 's|^\./||; s|/go.mod$||' | sort)
in_tag=$(grep -o 'SUBMODULES="[^"]*"' .github/workflows/release.yml | sed 's/^SUBMODULES="//; s/"$//' | tr ' ' '\n' | sort)
in_mage=$(sed -n 's/^var releaseSubModules = \[\]string{\(.*\)}/\1/p' magefiles/mage_release.go | tr -d '" ' | tr ',' '\n' | sort)
expect_ok "sub-modules on disk == release.yml SUBMODULES" test "$published" = "$in_tag"
expect_ok "sub-modules on disk == releaseSubModules (PrepRelease/CheckRelease)" test "$published" = "$in_mage"
for m in $published; do
  expect_ok "release.yml asks the proxy for $m" grep -q "$MODPATH/$m" .github/workflows/release.yml
  expect_ok "$m has a replace to the tree" grep -q '^replace github.com/goceleris/celeris => ' "$m/go.mod"
done
SUBS=$published

##########################################################################
step "1 a version the gate refuses is refused"
fresh gate
if VERSION=v1.6.0-rehearsal.1 mage "$T/gate" PrepRelease >"$T/gate.out" 2>&1; then
  bad "PrepRelease accepted v1.6.0-rehearsal.1"
else
  ok "PrepRelease refuses v1.6.0-rehearsal.1"; tail -n 2 "$T/gate.out" | sed 's/^/      /'
fi

##########################################################################
step "2 PrepRelease and the release-mode CheckRelease"
fresh main
export VERSION
cd "$T/main" || exit 2
git checkout -q -b prep
expect_red "CheckRelease in release mode fails before PrepRelease (stamps and pins are older)" mage "$T/main" CheckRelease
if [ -n "${PINMOVE_SKIP_PREP:-}" ]; then
  echo "  (PINMOVE_SKIP_PREP: the pin move is skipped on purpose)"
else
  mage "$T/main" PrepRelease
fi
if grep -q '^<!-- TODO(release):' README.md; then
  expect_red "CheckRelease in release mode fails while the README placeholder remains" mage "$T/main" CheckRelease
  # What a person does: replace the placeholder with prose.
  sed -i.bak 's|^<!-- TODO(release):.*-->$|Rehearsal release prose.|' README.md && rm -f README.md.bak
fi
if [ -n "${PINMOVE_SKIP_PREP:-}" ]; then
  expect_red "CheckRelease in release mode fails with the pins unmoved" mage "$T/main" CheckRelease
else
  expect_ok "CheckRelease in release mode passes after PrepRelease" mage "$T/main" CheckRelease
fi
if [ -n "${PINMOVE_SKIP_PREP:-}" ]; then
  git commit -q --allow-empty -m "prep $VERSION (nothing moved: negative control)"
else
  git commit -q -am "prep $VERSION"
fi
git diff --stat HEAD~1 | tail -n 8
for m in $SUBS; do
  printf '    %-24s pins %s\n' "$m" "$(grep -o "celeris v[0-9][^ ]*" "$m/go.mod")"
done
for m in $SUBS; do
  if (cd "$m" && go mod tidy -diff >/dev/null 2>&1); then ok "$m: go mod tidy -diff is clean"; else bad "$m: go mod tidy -diff reports changes"; fi
  if (cd "$m" && go build ./... && go vet ./... >/dev/null); then ok "$m: builds in tree (replace ../../)"; else bad "$m: does not build in tree"; fi
done
PREP=$(git rev-parse HEAD)
local_origin "$T/main" || exit 2
git push -q origin prep:main
cd "$ROOT" || exit 2

##########################################################################
step "3 the window before the root tag: a consumer at the prep commit's pseudo-version"
forget_celeris
for m in $SUBS; do
  d=$T/c-window-${m##*/}
  if consumer "$d" "$m" "$PREP"; then
    bad "$m@${PREP:0:12} resolved before $VERSION exists"
  else
    ok "$m@${PREP:0:12} does not resolve before the root tag exists"; first_lines "$d/get.err" 3
  fi
done

##########################################################################
step "9 control: sub-module tags without a root tag must not resolve"
for m in $SUBS; do git --git-dir="$T/main.git" tag "$m/$VERSION" "$PREP"; done
forget_celeris
for m in $SUBS; do
  d=$T/c-noroot-${m##*/}
  if consumer "$d" "$m" "$VERSION"; then
    bad "$m@$VERSION resolved with no root tag"
  else
    ok "$m@$VERSION does not resolve with no root tag"; first_lines "$d/get.err" 2
  fi
done
for m in $SUBS; do git --git-dir="$T/main.git" tag -d "$m/$VERSION" >/dev/null; done

##########################################################################
step "4 root tag, then release.yml's 'Create sub-module tags' step (extracted, unchanged)"
git --git-dir="$T/main.git" tag "$VERSION" "$PREP"
rm -rf "$T/tagger"; git clone -q "$T/main.git" "$T/tagger"
git -C "$T/tagger" checkout -q "$VERSION"
# The step's run: block, de-indented, ends at the next job.
awk '/- name: Create sub-module tags/{f=1; next} f && /^ *run: \|/{r=1; next} r && /^  [a-z-]+:$/{exit} r{print}' \
  .github/workflows/release.yml | sed 's/^          //' >"$T/tag-step.sh"
if [ -s "$T/tag-step.sh" ]; then ok "extracted the tag step ($(wc -l <"$T/tag-step.sh") lines)"; else bad "could not extract the tag step from release.yml"; fi
local_origin "$T/tagger" || exit 2
if grep -Eq 'github\.com|https?://|TOKEN|set-url|remote add' "$T/tag-step.sh"; then
  # The step is run unchanged, so a change to it that names a URL or a token is read by a person first.
  bad "release.yml's tag step now names a URL or a token; read it before the rehearsal runs it"
elif (cd "$T/tagger" && VERSION=$VERSION bash -e "$T/tag-step.sh" 2>&1 | sed 's/^/      /'; exit "${PIPESTATUS[0]}"); then
  ok "tag step exited 0"
else
  bad "tag step failed"
fi
for m in $SUBS; do
  tgt=$(git --git-dir="$T/main.git" rev-parse "$m/$VERSION^{commit}" 2>/dev/null || true)
  expect_ok "$m/$VERSION exists in the remote at the root tag's commit" test "$tgt" = "$(git --git-dir="$T/main.git" rev-parse "$VERSION^{commit}")"
done

##########################################################################
step "5 external consumer of each sub-module (VCS fetch from the local remote)"
forget_celeris
for m in $SUBS; do
  d=$T/c-${m##*/}
  if consumer "$d" "$m" "$VERSION"; then ok "$m@$VERSION: consumer builds"; else bad "$m@$VERSION: consumer failed"; sed 's/^/      /' "$d/get.err"; fi
  if resolves_to "$d" "$VERSION"; then ok "$m@$VERSION pulls celeris $VERSION"; else bad "$m@$VERSION does not pull celeris $VERSION"; fi
  if (cd "$d" && GOPROXY=off GOFLAGS="-modcacherw -mod=mod" go build -o /dev/null . 2>/dev/null); then ok "$m consumer rebuilds with GOPROXY=off from the module cache"; else bad "$m consumer does not rebuild with GOPROXY=off"; fi
  celeris_sums "$d" | sed 's/^/      go.sum: /'
done

##########################################################################
step "6 release.yml's 'Verify module availability' query (go list -m MODULE@VERSION)"
mkdir -p "$T/q" && printf 'module example.com/q\n\ngo %s\n' "$GOVER" >"$T/q/go.mod"
for mod in $MODPATH $(for m in $SUBS; do echo "$MODPATH/$m"; done); do
  if (cd "$T/q" && go list -m "$mod@$VERSION" >/dev/null 2>&1); then ok "go list -m $mod@$VERSION"; else bad "go list -m $mod@$VERSION"; fi
done

##########################################################################
step "7 the same consumers over GOPROXY=file:// (fresh GOMODCACHE, GOSUMDB=off, no VCS)"
PROXY=$T/proxy
cp -R "$GOMODCACHE/cache/download" "$PROXY"
{ ls "$PROXY/$MODPATH/@v/" 2>/dev/null || true; } | sed -n '1,12p' | sed 's/^/      proxy: /'
for m in $SUBS; do
  d=$T/o-${m##*/}
  if (export GOMODCACHE=$T/modcache-offline GOPROXY=file://$PROXY GOSUMDB=off; unset GOPRIVATE; consumer "$d" "$m" "$VERSION"); then
    ok "$m@$VERSION resolves from the file:// proxy"
  else
    bad "$m@$VERSION does not resolve from the file:// proxy"; sed 's/^/      /' "$d/get.err"
  fi
  if resolves_to "$d" "$VERSION"; then ok "$m via file:// proxy: celeris $VERSION"; else bad "$m via file:// proxy: celeris not $VERSION"; fi
  expect_ok "$m: celeris go.sum lines equal the VCS-fetch run's" test "$(celeris_sums "$d")" = "$(celeris_sums "$T/c-${m##*/}")"
done
chmod -R u+w "$T/modcache-offline" 2>/dev/null; rm -rf "$T/modcache-offline"

##########################################################################
step "8 control: the same tags on a tree whose pins were NOT moved"
fresh ctl
CTL=$(git --git-dir="$T/ctl.git" rev-parse refs/heads/main)
git --git-dir="$T/ctl.git" tag "$VERSION" "$CTL"
for m in $SUBS; do git --git-dir="$T/ctl.git" tag "$m/$VERSION" "$CTL"; done
forget_celeris
for m in $SUBS; do
  d=$T/c-ctl-${m##*/}
  consumer "$d" "$m" "$VERSION" || true
  echo "      $m@$VERSION on the unmoved tree: celeris $(resolved_root "$d") (go.mod pin: $(git --git-dir="$T/ctl.git" show "$VERSION:$m/go.mod" | grep -o 'celeris v[0-9][^ ]*'))"
  expect_red "$m: 'pulls celeris $VERSION' on the unmoved tree" resolves_to "$d" "$VERSION"
done

##########################################################################
step "10 retraction probe: a bad sub-module version, retracted by the next one"
fresh retr
BAD=v1.99.0
FIX=v1.99.1
m=middleware/otel
git --git-dir="$T/retr.git" tag "$m/$BAD" "$(git --git-dir="$T/retr.git" rev-parse refs/heads/main)"
(
  cd "$T/retr" && git checkout -q -b fix \
    && printf '\nretract %s // rehearsal: pinned the wrong celeris\n' "$BAD" >>"$m/go.mod" \
    && git commit -q -am "retract $BAD in $m" && git tag "$m/$FIX" && local_origin "$T/retr" && git push -q origin "$m/$FIX"
)
forget_celeris
mkdir -p "$T/r" && printf 'module example.com/r\n\ngo %s\n' "$GOVER" >"$T/r/go.mod"
listed=$(cd "$T/r" && go list -m -versions "$MODPATH/$m" 2>&1 || true)
listed_all=$(cd "$T/r" && go list -m -versions -retracted "$MODPATH/$m" 2>&1 || true)
echo "$listed" | sed 's/^/      default:   /'
echo "$listed_all" | sed 's/^/      -retracted: /'
if grep -qw "$BAD" <<<"$listed"; then bad "retracted $BAD is still listed by default"; else ok "retracted $BAD is hidden from the default version list"; fi
if grep -qw "$BAD" <<<"$listed_all"; then ok "-retracted still lists $BAD"; else bad "-retracted does not list $BAD"; fi
(cd "$T/r" && GOFLAGS="-modcacherw -mod=mod" go get "$MODPATH/$m@$BAD" 2>&1 | sed 's/^/      go get @bad: /') || true

##########################################################################
echo
echo "pass=$pass fail=$fail (version $VERSION; temp dir $( [ -n "${PINMOVE_KEEP:-}" ] && echo "kept at $T" || echo removed ))"
if [ "$fail" -ne 0 ]; then exit 1; fi
