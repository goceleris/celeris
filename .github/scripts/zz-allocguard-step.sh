set -o pipefail
# "<package> <top-level tests that must PASS, as a regex alternation>"
entries=(
  'celeristest TestNewContextZeroAlloc'
  'driver/memcached TestAllocBudgets'
  'driver/postgres TestAllocBudgets|TestAllocBudgetsPreparedExec'
  'driver/redis TestAllocBudgets'
  'middleware/cache TestAllocBudgets'
  'middleware/idempotency TestAllocBudgets'
  'middleware/overload TestAllocBudgets'
  'middleware/ratelimit TestAllowUnderLimitZeroAlloc'
  'middleware/timeout TestNoTimeoutCommonPathSingleAlloc'
  'middleware/sse TestBrokerFanOutFormatsOnce|TestRingBufferAppendAllocs|TestSlowClientSendNonFullQueueAllocs|TestSlowClientDropPathAllocs888'
  'middleware/websocket TestHubBroadcastFormatsOnce'
)
failed=0
bad=0
total=0
: >/tmp/allocguard.log
for entry in "${entries[@]}"; do
  pkg=${entry%% *}
  names=${entry#* }
  want=$(( $(tr '|' '\n' <<<"$names" | wc -l) ))
  log="/tmp/allocguard-${pkg//\//-}.log"
  go test -count=1 -timeout=300s -v -run "^(${names})\$" "./${pkg}/" </dev/null 2>&1 | tee "$log" || failed=$((failed + 1))
  cat "$log" >>/tmp/allocguard.log
  ran=$(grep -cE "^=== RUN   (${names})\$" "$log" || true)
  passed=$(grep -cE "^--- PASS: (${names}) \(" "$log" || true)
  total=$((total + passed))
  echo "alloc guards ${pkg}: want $want, ran $ran, passed $passed"
  if [ "$ran" -ne "$want" ] || [ "$passed" -ne "$want" ]; then bad=$((bad + 1)); fi
done
skipped=$("$GITHUB_WORKSPACE"/.github/scripts/skip-count.sh /tmp/allocguard.log)
faillines=$(grep -cE '^[[:space:]]*--- FAIL' /tmp/allocguard.log || true)
echo "alloc guards: top-level PASS $total (want 15), packages with a wrong tally $bad, SKIP lines $skipped, FAIL lines $faillines, go test runs failed $failed (want 0)"
if [ "$total" -ne 15 ] || [ "$bad" -ne 0 ] || [ "$skipped" -ne 0 ] || [ "$faillines" -ne 0 ] || [ "$failed" -ne 0 ]; then
  echo "expected the 15 guard tests of 11 packages to run and PASS with no SKIP or FAIL line --"
  echo "was one renamed or removed, did one skip (alloc counts are skipped under -race, coverage and -short), or did one fail?"
  exit 1
fi
