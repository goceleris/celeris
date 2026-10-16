#!/usr/bin/env bash
# bench-ab.sh: settle a benchmark flag (a CodSpeed regression, or any "is this
# slower?") with an interleaved A/B plus an A/A floor, one process per
# observation (celeris#725). A B-vs-A delta inside the floor is inconclusive:
# repeat or extend the rounds until the floor is below the flagged difference.
# usage: .github/scripts/bench-ab.sh <base-ref> <head-ref> <package> <bench-regex> [rounds]
#   e.g. .github/scripts/bench-ab.sh origin/main HEAD ./middleware \
#          '^BenchmarkChain(Baseline|MinimalAPI|PreRoutingOnly)$' 20
#
# What it does:
#   1. Checks out <base-ref> and <head-ref> in temporary worktrees. Refs are
#      commits: uncommitted edits are not measured, so commit them first.
#   2. Builds <package>'s test binary at both, with -trimpath, twice: for
#      linux/arm64 (CodSpeed's runner) and for this machine. It says whether
#      each pair is byte-identical. Identical linux/arm64 binaries mean any
#      difference CodSpeed reported between the two refs came from its
#      runner. The host pair only speaks for this host: a change to
#      Linux-only code (the engines, internal/conn) leaves macOS binaries
#      identical and linux/arm64 ones different.
#   3. Copies the host A to A2: a byte-identical third arm, whose spread
#      against A is the noise floor of this machine.
#   4. Runs <rounds> rounds (default 20). A round runs A, B and A2 once each,
#      a fresh process per run (-test.count=1), with the arm order rotated
#      every round so that no arm always runs first or last.
#   5. Checks that A, B and A2 each hold results and name the same benchmarks
#      (the -N suffix stripped), after round 1 and again after the last round.
#      A side with no results, or two sides that name different benchmarks,
#      exits 2 (after round 1, if it shows then) before any benchstat table is
#      printed: a regex that matches only in the head would otherwise give an
#      empty A and a table that looks like a result.
#   6. Prints benchstat for A vs B and for A vs A2.
#
# Reading it: a B-vs-A delta that is not larger than the A-vs-A2 delta is
# inconclusive: repeat or extend the rounds until the floor is below the
# flagged difference. That holds for B/op and allocs/op too: sync.Pool and the
# GC make them vary a little between runs of one binary, so compare them with
# A2 as well.
#
# The timings describe the machine they ran on. A build for your laptop has
# its own code layout, so it answers "did the code on this path get slower",
# not "why did the CodSpeed arm64 runner move". For the runner's layout, run
# this on an arm64 Linux host. Run it on a quiet machine: nothing else busy,
# on AC power.
#
# Environment: BENCHTIME (default 1s, as CodSpeed runs it), OUT (a directory
# to keep the raw outputs in; it must not hold an earlier run's A.txt, B.txt
# or A2.txt; default a temporary one, removed on exit).
# benchstat is the one .github/tools/go.mod pins; `go tool` builds it on first
# use, so there is nothing to install (celeris#838). GOWORK=off because go
# refuses -modfile in workspace mode, which a contributor's go.work (.gitignore
# lists it) turns on.
set -euo pipefail

if [ "$#" -lt 4 ]; then
  sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
fi
base=$1 head=$2 pkg=$3 re=$4 rounds=${5:-20}
benchtime=${BENCHTIME:-1s}
if ! [[ $rounds =~ ^[1-9][0-9]*$ ]]; then
  echo "rounds must be a positive integer, not '$rounds'" >&2
  exit 2
fi

repo=$(git rev-parse --show-toplevel)
benchstat() { GOWORK=off go tool -modfile="$repo/.github/tools/go.mod" benchstat "$@"; }
GOWORK=off go tool -modfile="$repo/.github/tools/go.mod" -n benchstat > /dev/null || { echo "cannot build benchstat from $repo/.github/tools/go.mod" >&2; exit 2; }
tmp=$(mktemp -d)
cleanup() {
  git -C "$repo" worktree remove --force "$tmp/a" 2> /dev/null || true
  git -C "$repo" worktree remove --force "$tmp/b" 2> /dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT
out=${OUT:-$tmp/out}
mkdir -p "$out"
for f in A B A2; do
  if [ -e "$out/$f.txt" ]; then
    echo "$out/$f.txt exists: an earlier run's results would be mixed into this one. Use an empty OUT." >&2
    exit 2
  fi
done

git -C "$repo" worktree add --detach --quiet "$tmp/a" "$base"
git -C "$repo" worktree add --detach --quiet "$tmp/b" "$head"

# Binary identity for CodSpeed's runner, then for this host.
(cd "$tmp/a" && GOOS=linux GOARCH=arm64 go test -trimpath -c -o "$tmp/A.linux-arm64.test" "$pkg")
(cd "$tmp/b" && GOOS=linux GOARCH=arm64 go test -trimpath -c -o "$tmp/B.linux-arm64.test" "$pkg")
(cd "$tmp/a" && go test -trimpath -c -o "$tmp/A.test" "$pkg")
(cd "$tmp/b" && go test -trimpath -c -o "$tmp/B.test" "$pkg")
cp "$tmp/A.test" "$tmp/A2.test"
host="$(go env GOOS)/$(go env GOARCH)"
for target in linux/arm64 "$host"; do
  if [ "$target" = linux/arm64 ]; then a=$tmp/A.linux-arm64.test b=$tmp/B.linux-arm64.test; else a=$tmp/A.test b=$tmp/B.test; fi
  if cmp -s "$a" "$b"; then
    echo "A ($base) and B ($head): the $pkg test binaries for $target are byte-identical."
  else
    echo "A ($base) and B ($head): the $pkg test binaries for $target differ."
  fi
done
echo "The timings below are for $host."

# Each side must hold results, and the sides must name the same benchmarks.
# Without this a regex that matches only in the head reaches benchstat with an
# empty A and prints a table for it. It runs after round 1, so a regex that
# matches nothing, or only in one ref, stops in seconds rather than after every
# round, and again after the last round.
names() { awk '/^Benchmark/ { sub(/-[0-9]+$/, "", $1); print $1 }' "$1" | LC_ALL=C sort -u; }
side_label() {
  case $1 in
    A) echo "A ($base)" ;;
    B) echo "B ($head)" ;;
    A2) echo "A2 (a copy of A)" ;;
  esac
}
check_names() {
  local ok=1 f
  for f in A B A2; do
    names "$out/$f.txt" > "$tmp/names.$f"
    if [ ! -s "$tmp/names.$f" ]; then
      echo "$(side_label "$f") has no benchmark result for '$re' in $out/$f.txt" >&2
      ok=0
    fi
  done
  for f in B A2; do
    if ! cmp -s "$tmp/names.A" "$tmp/names.$f"; then
      echo "$(side_label "$f") names different benchmarks from A for '$re':" >&2
      LC_ALL=C comm -23 "$tmp/names.A" "$tmp/names.$f" | sed 's/^/  only in A: /' >&2
      LC_ALL=C comm -13 "$tmp/names.A" "$tmp/names.$f" | sed "s/^/  only in $f: /" >&2
      ok=0
    fi
  done
  if [ "$ok" = 0 ]; then
    echo "nothing comparable: fix the regex or the refs, then run again." >&2
    exit 2
  fi
}

# A test binary runs in its package directory (testdata, relative paths).
dir_a=$tmp/a/${pkg#./}
dir_b=$tmp/b/${pkg#./}
arms=(A B A2)
for ((r = 1; r <= rounds; r++)); do
  for ((k = 0; k < 3; k++)); do
    arm=${arms[$(((r + k) % 3))]}
    dir=$dir_a
    [ "$arm" = B ] && dir=$dir_b
    (cd "$dir" && "$tmp/$arm.test" -test.run '^$' -test.bench "$re" \
      -test.benchtime "$benchtime" -test.count 1 -test.benchmem) >> "$out/$arm.txt"
  done
  echo "round $r/$rounds done"
  if [ "$r" = 1 ]; then check_names; fi
done
check_names

echo
echo "== B ($head) vs A ($base)"
benchstat A="$out/A.txt" B="$out/B.txt"
echo
echo "== A2 vs A: the same binary twice, this machine's floor"
benchstat A="$out/A.txt" A2="$out/A2.txt"
