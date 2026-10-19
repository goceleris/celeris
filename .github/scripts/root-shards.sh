#!/usr/bin/env bash
# Root-package test shards (celeris#890).
#
# "The root package's tests" are the tests of two packages: the root package
# itself (the tests that use unexported symbols, the godoc examples, the
# benchmarks CodSpeed tracks) and ./test/server, where the tests that need only
# the exported API live (so the root directory holds the library, not 60 test
# files of one kind). `pkgs` prints both; a test name is unique across the two.
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
#   root-shards.sh pkgs                the packages the root tests live in (". ./test/server"),
#                                      for `go test`; run them with -p 1 in a shard (one
#                                      process at a time keeps the runner's one memlock budget)
#   root-shards.sh names <shard>       the shard's tests, one per line
#   root-shards.sh run-regex <shard>   ^(A|B|...)$   for `go test -run`
#   root-shards.sh skip-regex          ^(every listed test)$   for `go test -skip`
#   root-shards.sh shards              the shard names, one per line
#   root-shards.sh check               the guard: every listed name is a test of
#                                      the root packages (`pkgs`), listed once, defined
#                                      once there and in no other package, -run selects exactly the
#                                      listed set of each shard, and every shard has
#                                      a job in ci.yml and test-coverage.yml (a workflow's
#                                      `shard: [...]` matrix equals the file's shards exactly)
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
# The packages the root tests live in. ROOT_PKGS overrides (the negative controls use it).
root_pkgs=${ROOT_PKGS:-". ./test/server"}

die() { echo "root-shards: $*" >&2; exit 1; }

# hint is appended to the guard's errors about the list: a pull request that
# renames or removes a listed test, or adds a test of a listed name to another
# package, lands here.
hint="; fix $file (its header says how: edit or drop the line, or rename the other test)"

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
  pkgs)
    echo "$root_pkgs" ;;
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
    [ -z "$dups" ] || die "listed more than once: $(paste -sd' ' - <<<"$dups")$hint"
    # The tests of the root packages as the CI run sees them (-race, linux).
    # shellcheck disable=SC2086  # flags and packages are word-split on purpose
    have=$(go test ${ROOT_TEST_FLAGS:--race} -list '.*' $root_pkgs | grep -E '^(Test|Example|Fuzz|Benchmark)' | sort)
    # Benchmarks are listed but `go test` here never runs them (no -bench).
    have_run=$(grep -vE '^Benchmark' <<<"$have" || true)
    [ -n "$have" ] || die "go test -list found no tests in the root packages ($root_pkgs)"
    missing=$(comm -13 <(echo "$have") <(awk '{print $2}' <<<"$all" | sort))
    [ -z "$missing" ] || die "listed but not a test of the root packages (renamed or removed?): $(paste -sd' ' - <<<"$missing")$hint"
    # A listed name defined in both root packages would run in its shard twice and
    # in the rest never (-skip skips both), so a name must be defined once there.
    twice=$(awk '{print $2}' <<<"$all" | sort | comm -12 - <(uniq -d <<<"$have"))
    [ -z "$twice" ] || die "listed and defined in more than one of the root packages ($root_pkgs): $(paste -sd' ' - <<<"$twice")$hint"
    # -skip applies to every package of the Unit job's one `go test` line, so a
    # listed name that another package also defines would be skipped there too.
    # Grep the test files of every other package of the root module (go list, so
    # sub-modules are out; test/ and the packages a workflow leaves out stay in,
    # which can only over-report, never hide a clash) for the name (a `func` line).
    # shellcheck disable=SC2086  # packages are word-split on purpose
    rootdirs=$(go list -f '{{.Dir}}' $root_pkgs)
    others=$(go list -f '{{.Dir}}' ./... | grep -vxF "$rootdirs" || true)
    others_files=$(for d in $others; do find "$d" -maxdepth 1 -name '*_test.go'; done)
    clash=""
    if [ -n "$others_files" ]; then
      while read -r _ t; do
        # shellcheck disable=SC2086  # one file per word
        hit=$(grep -lE "^func ${t}\(" $others_files | sed "s#^$PWD/##" || true)
        [ -z "$hit" ] || clash="$clash $t($(tr '\n' ',' <<<"$hit"))"
      done <<<"$all"
    fi
    [ -z "$clash" ] || die "a listed name is also defined outside the root package, so -skip would skip that one too:$clash$hint"
    # -run with each shard's regex selects exactly that shard's names.
    for s in $(entries | awk '{print $1}' | sort -u); do
      want=$(awk -v s="$s" '$1 == s {print $2}' <<<"$all" | sort)
      # shellcheck disable=SC2086
      got=$(go test ${ROOT_TEST_FLAGS:--race} -list "$("$0" run-regex "$s")" $root_pkgs | grep -E '^(Test|Example|Fuzz|Benchmark)' | sort)
      [ "$want" = "$got" ] || die "shard $s: -run selects [$(tr '\n' ' ' <<<"$got")], the file lists [$(tr '\n' ' ' <<<"$want")]$hint"
    done
    # Every shard of the file has a job: each workflow's matrix names exactly the
    # file's shards, else a listed test would run nowhere (a silent drop).
    file_shards=$(entries | awk '{print $1}' | sort -u | paste -sd' ' -)
    for wf in ${WORKFLOWS:-.github/workflows/ci.yml .github/workflows/test-coverage.yml}; do
      lines=$(grep -E '^[[:space:]]+shard: \[' "$wf" || true)
      [ "$(grep -c . <<<"$lines")" -ge 1 ] || die "$wf has no \`shard: [...]\` matrix line"
      while IFS= read -r ln; do
        m=$(sed -E 's/.*\[(.*)\].*/\1/' <<<"$ln" | tr -d ' ' | tr ',' '\n' | sort -u | paste -sd' ' -)
        [ "$m" = "$file_shards" ] || die "$wf: matrix shards [$m] differ from the shards of $file [$file_shards]; give each shard of the file a job and its name in the \`shard: [...]\` matrix of both workflows"
      done <<<"$lines"
    done
    listed=$(wc -l <<<"$all")
    kinds=$(grep -oE '^(Test|Example|Fuzz|Benchmark)' <<<"$have" | sort | uniq -c | awk '{printf "%s %s, ", $2, $1}')
    echo "root-shards: ok: ${kinds%, } names in the root packages (Benchmarks are never run here), $listed Tests in shards ($(entries | awk '{print $1}' | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}')), $(( $(wc -l <<<"$have_run") - listed )) Test/Fuzz/Example names run by the rest (-skip)"
    ;;
  tally-shard)
    s=${2:?shard}; log=${3:?log}
    want=$(awk -v s="$s" '$1 == s {print $2}' < <(entries) | sort)
    [ -n "$want" ] || die "shard $s lists no tests"
    ran=$(grep -E '^=== RUN   [^/ ]+$' "$log" | sed -E 's/^=== RUN   //' | sort -u)
    passed=$(grep -E '^--- PASS: [^/ ]+ \(' "$log" | sed -E 's/^--- PASS: ([^ ]+) .*/\1/' | sort -u)
    # A joined `--- SKIP:` line (celeris#890) is no SKIP line to the grep below, and a subtest's can
    # leave its parent's PASS line standing: skip-count.sh fails on it.
    "$(dirname "$0")/skip-count.sh" "$log" >/dev/null
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
