#!/usr/bin/env bash
# Root-package test shards (celeris#890).
#
# The root package's tests run one after another and took ~590 s on the runner
# under -race, which made it the longest step of the Unit and Coverage jobs. The
# slow tests are listed in .github/root-test-shards.txt, one `<shard> <Test>` per
# line. A shard runs in a job of its own (its own runner, so its own 8 MiB
# RLIMIT_MEMLOCK: two processes of one runner would share one io_uring ring
# budget). The job that has always run the root package runs the rest with
# `-skip`, so a test that is in no list still runs there: a new test cannot be
# dropped by omission, only by being listed here.
#
#   root-shards.sh names <shard>       the shard's tests, one per line
#   root-shards.sh run-regex <shard>   ^(A|B|...)$   for `go test -run`
#   root-shards.sh skip-regex          ^(every listed test)$   for `go test -skip`
#   root-shards.sh shards              the shard names, one per line
#   root-shards.sh check               the guard: every listed name is a test of
#                                      the root package, listed once, defined in
#                                      no other package, -run selects exactly the
#                                      listed set of each shard, and every shard has
#                                      a job in ci.yml and test-coverage.yml
#   root-shards.sh tally-shard <shard> <log>
#                                      a shard's `go test -v` log holds every
#                                      listed test as one top-level PASS and
#                                      nothing else (no extra test, no SKIP, no FAIL)
#   root-shards.sh tally-rest <log>    the rest's log ran none of the listed tests
#
# Run it from the repository root. SHARDS_FILE overrides the list (the negative
# controls use it); ROOT_TEST_FLAGS overrides the flags of the `go test -list`
# the guard makes (default -race, the CI shape, because the race761_on/off pair
# and the _linux files change the set of tests).
set -euo pipefail

file=${SHARDS_FILE:-.github/root-test-shards.txt}

die() { echo "root-shards: $*" >&2; exit 1; }

# entries prints "<shard> <test>" for every non-comment line, after the syntax
# check: two fields, a lower-case shard name, a Test name.
entries() {
  [ -f "$file" ] || die "$file not found (run from the repository root)"
  awk -v f="$file" '
    /^[[:space:]]*(#|$)/ { next }
    {
      if (NF != 2 || $1 !~ /^[a-z][a-z0-9]*$/ || $2 !~ /^Test[A-Za-z0-9_]*$/) {
        printf "root-shards: %s:%d: want \"<shard> <TestName>\", got: %s\n", f, NR, $0 > "/dev/stderr"; bad = 1
      }
      print $1, $2
    }
    END { exit bad }' "$file"
}

regex_of() { # stdin: names
  local names
  names=$(paste -sd'|' -)
  [ -n "$names" ] || die "empty test list for ${1:-the request}"
  printf '^(%s)$\n' "$names"
}

cmd=${1:-}
case "$cmd" in
  shards)
    entries | awk '{print $1}' | sort -u ;;
  names)
    s=${2:?shard}
    entries | awk -v s="$s" '$1 == s {print $2}' ;;
  run-regex)
    s=${2:?shard}
    entries | awk -v s="$s" '$1 == s {print $2}' | regex_of "shard $s" ;;
  skip-regex)
    entries | awk '{print $2}' | regex_of "the shards" ;;
  check)
    all=$(entries) || exit 1
    [ -n "$all" ] || die "$file lists no tests"
    dups=$(awk '{print $2}' <<<"$all" | sort | uniq -d)
    [ -z "$dups" ] || die "listed more than once: $(tr '\n' ' ' <<<"$dups")"
    # The tests of the root package as the CI run sees them (-race, linux).
    # shellcheck disable=SC2086  # flags are word-split on purpose
    have=$(go test ${ROOT_TEST_FLAGS:--race} -list '.*' . | grep -E '^(Test|Example|Fuzz|Benchmark)' | sort)
    [ -n "$have" ] || die "go test -list found no tests in the root package"
    missing=$(comm -13 <(echo "$have") <(awk '{print $2}' <<<"$all" | sort))
    [ -z "$missing" ] || die "listed but not a test of the root package (renamed or removed?): $(tr '\n' ' ' <<<"$missing")"
    # -skip applies to every package of the Unit job's one `go test` line, so a
    # listed name that another package also defines would be skipped there too.
    # Grep every other package's test files for the name (a single `func` line).
    clash=""
    while read -r _ t; do
      hit=$(grep -rlE --include='*_test.go' "^func ${t}\(" . 2>/dev/null | grep -vE '^\./[^/]+$' || true)
      [ -z "$hit" ] || clash="$clash $t($(tr '\n' ',' <<<"$hit"))"
    done <<<"$all"
    [ -z "$clash" ] || die "a listed name is also defined outside the root package, so -skip would skip that one too:$clash"
    # -run with each shard's regex selects exactly that shard's names.
    for s in $(entries | awk '{print $1}' | sort -u); do
      want=$(awk -v s="$s" '$1 == s {print $2}' <<<"$all" | sort)
      # shellcheck disable=SC2086
      got=$(go test ${ROOT_TEST_FLAGS:--race} -list "$("$0" run-regex "$s")" . | grep -E '^(Test|Example|Fuzz|Benchmark)' | sort)
      [ "$want" = "$got" ] || die "shard $s: -run selects [$(tr '\n' ' ' <<<"$got")], the file lists [$(tr '\n' ' ' <<<"$want")]"
    done
    # Every shard of the file has a job: each workflow's matrix names exactly the
    # file's shards, else a listed test would run nowhere (a silent drop).
    file_shards=$(entries | awk '{print $1}' | sort -u | paste -sd' ' -)
    for wf in ${WORKFLOWS:-.github/workflows/ci.yml .github/workflows/test-coverage.yml}; do
      lines=$(grep -E '^[[:space:]]+shard: \[' "$wf" || true)
      [ "$(grep -c . <<<"$lines")" -ge 1 ] || die "$wf has no \`shard: [...]\` matrix line"
      while IFS= read -r ln; do
        m=$(sed -E 's/.*\[(.*)\].*/\1/' <<<"$ln" | tr -d ' ' | tr ',' '\n' | sort -u | paste -sd' ' -)
        [ "$m" = "$file_shards" ] || die "$wf: matrix shards [$m] differ from the shards of $file [$file_shards]"
      done <<<"$lines"
    done
    total=$(wc -l <<<"$have")
    listed=$(wc -l <<<"$all")
    echo "root-shards: ok: $total tests in the root package, $listed in shards ($(entries | awk '{print $1}' | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}')), $((total - listed)) run by the rest (-skip)"
    ;;
  tally-shard)
    s=${2:?shard}; log=${3:?log}
    want=$(awk -v s="$s" '$1 == s {print $2}' < <(entries) | sort)
    [ -n "$want" ] || die "shard $s lists no tests"
    ran=$(grep -E '^=== RUN   [^/ ]+$' "$log" | sed -E 's/^=== RUN   //' | sort -u)
    passed=$(grep -E '^--- PASS: [^/ ]+ \(' "$log" | sed -E 's/^--- PASS: ([^ ]+) .*/\1/' | sort -u)
    bad=$(grep -cE '^[[:space:]]*--- (SKIP|FAIL): ' "$log" || true)
    echo "shard $s: want $(wc -l <<<"$want") tests, ran $(wc -l <<<"$ran"), passed $(wc -l <<<"$passed"), SKIP/FAIL lines $bad (want 0)"
    [ "$want" = "$ran" ] || { echo "ran differs from the list (< only listed, > only ran):"; diff <(echo "$want") <(echo "$ran") || true; exit 1; }
    [ "$want" = "$passed" ] || { echo "passed differs from the list (< only listed, > only passed):"; diff <(echo "$want") <(echo "$passed") || true; exit 1; }
    [ "$bad" -eq 0 ] || { grep -E '^[[:space:]]*--- (SKIP|FAIL): ' "$log" | head -20; exit 1; }
    ;;
  tally-rest)
    log=${2:?log}
    names=$(entries | awk '{print $2}' | paste -sd'|' -)
    n=$(grep -cE "^=== RUN   (${names})\$" "$log" || true)
    echo "rest: listed shard tests that ran in this log: $n (want 0)"
    [ "$n" -eq 0 ] || { grep -E "^=== RUN   (${names})\$" "$log" | head; exit 1; }
    ;;
  *)
    sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
    exit 2 ;;
esac
