#!/bin/bash
# Negative control for Tier 1 (celeris#583, review correction 5): the two
# checked-in backpressure oracles run ONCE each per engine under the probe
# (drain on, hook unset, CLOSE-PROBE lines on stderr). Expected: inq_before==0
# on ~100% of their closes, which is why they are not used as the A/B.
WT=/Users/fuming/Documents/github/celeris/celeris/.claude/worktrees/wf-583
OUT=$WT/scratchpad/logs/tier1/control_oracles
mkdir -p "$OUT"
docker run --rm --cpus 4 --security-opt seccomp=unconfined --ulimit memlock=134217728:134217728 \
  -v "$WT":/src -w /src -v /Users/fuming/go/pkg/mod:/go/pkg/mod -v gocache484:/root/.cache/go-build \
  golang:1.27 bash -c '
OUT=/src/scratchpad/logs/tier1/control_oracles
go test -c -tags celeris_closeprobe -o /tmp/ws583.test ./middleware/websocket/ > "$OUT/build.log" 2>&1 || { echo BUILD-FAILED; exit 1; }
for eng in epoll io_uring; do
  for tst in TestBackpressureInboundSequenceIntegrity TestBackpressurePauseDoesNotCancelInflightSend; do
    log="$OUT/${tst}_${eng}.log"
    start=$(date +%s)
    CELERIS_DEBUG_CLOSE_PROBE=1 /tmp/ws583.test -test.run "^${tst}\$/^${eng}\$" -test.v -test.count=1 -test.timeout 20m > "$log" 2>&1
    rc=$?
    echo "RUN-EXIT=$rc elapsed=$(( $(date +%s) - start ))s" >> "$log"
    total=$(grep -c "^CLOSE-PROBE" "$log")
    pos=$(grep "^CLOSE-PROBE" "$log" | grep -vc "inq_before=0 ")
    echo "$tst $eng exit=$rc elapsed=$(( $(date +%s) - start ))s closeProbes=$total inqBeforePos=$pos"
  done
done
'
