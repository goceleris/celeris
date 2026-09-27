#!/usr/bin/env bash
# bench-ab.sh: settle a benchmark flag (a CodSpeed regression, or any "is this
# slower?") with an interleaved A/B plus an A/A floor, one process per
# observation (celeris#725).
#
# usage: .github/scripts/bench-ab.sh <base-ref> <head-ref> <package> <bench-regex> [rounds]
#   e.g. .github/scripts/bench-ab.sh origin/main HEAD ./middleware \
#          '^BenchmarkChain(Baseline|MinimalAPI|PreRoutingOnly)$' 20
#
# What it does:
#   1. Builds <package>'s test binary at <base-ref> (arm A) and at <head-ref>
#      (arm B), each in a temporary worktree, with -trimpath, and says whether
#      the two binaries are byte-identical. If they are, any difference a
#      benchmark service reported between them came from its machine.
#   2. Copies A to A2: a byte-identical third arm, whose spread against A is
#      the noise floor of this machine.
#   3. Runs <rounds> rounds (default 20). A round runs A, B and A2 once each,
#      a fresh process per run (-test.count=1), with the arm order rotated
#      every round so that no arm always runs first or last.
#   4. Prints benchstat for A vs B and for A vs A2.
#
# Reading it: a B-vs-A delta that is not larger than the A-vs-A2 delta is not
# a change. B/op and allocs/op do not depend on the machine: if they moved,
# the code moved.
#
# The numbers describe the machine they ran on. A build for your laptop has
# its own code layout, so it answers "did the code on this path get slower",
# not "why did the CodSpeed arm64 runner move". For the runner's layout, run
# this on an arm64 Linux host. Run it on a quiet machine: nothing else busy,
# on AC power.
#
# Environment: BENCHTIME (default 1s, as CodSpeed runs it), OUT (a directory
# to keep the raw outputs in; default a temporary one, removed on exit).
# Needs benchstat: go install golang.org/x/perf/cmd/benchstat@latest
set -euo pipefail

if [ "$#" -lt 4 ]; then
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
fi
base=$1 head=$2 pkg=$3 re=$4 rounds=${5:-20}
benchtime=${BENCHTIME:-1s}
command -v benchstat > /dev/null || { echo "benchstat not found: go install golang.org/x/perf/cmd/benchstat@latest" >&2; exit 2; }

repo=$(git rev-parse --show-toplevel)
tmp=$(mktemp -d)
cleanup() {
  git -C "$repo" worktree remove --force "$tmp/a" 2> /dev/null || true
  git -C "$repo" worktree remove --force "$tmp/b" 2> /dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT
out=${OUT:-$tmp/out}
mkdir -p "$out"

git -C "$repo" worktree add --detach --quiet "$tmp/a" "$base"
git -C "$repo" worktree add --detach --quiet "$tmp/b" "$head"
(cd "$tmp/a" && go test -trimpath -c -o "$tmp/A.test" "$pkg")
(cd "$tmp/b" && go test -trimpath -c -o "$tmp/B.test" "$pkg")
cp "$tmp/A.test" "$tmp/A2.test"
if cmp -s "$tmp/A.test" "$tmp/B.test"; then
  echo "A ($base) and B ($head): the $pkg test binaries are byte-identical."
else
  echo "A ($base) and B ($head): the $pkg test binaries differ."
fi

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
done

echo
echo "== B ($head) vs A ($base)"
benchstat A="$out/A.txt" B="$out/B.txt"
echo
echo "== A2 vs A: the same binary twice, this machine's floor"
benchstat A="$out/A.txt" A2="$out/A2.txt"
