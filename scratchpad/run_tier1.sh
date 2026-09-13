#!/bin/bash
# Tier 1 (celeris#583): server-initiated detached close on a flooding,
# non-reading peer; drain-on vs drain-off per engine.
#   RUNS=24 ENGINES="epoll io_uring io_uring_multishot" ARMS="on off" PROBE=1 LABEL=main ./run_tier1.sh
# The drain-off arm is produced ONLY by CELERIS_DEBUG_SKIP_CLOSE_DRAIN=1 in a
# binary built with -tags celeris_closeprobe (sockopts.CloseDrain returns
# without reading). PROBE=0 is the perturbation check (no CLOSE-PROBE records,
# clients fall back to a fixed wait before reading).
WT=/Users/fuming/Documents/github/celeris/celeris/.claude/worktrees/wf-583
RUNS=${RUNS:-1}
ENGINES=${ENGINES:-"epoll io_uring io_uring_multishot"}
ARMS=${ARMS:-"on off"}
PROBE=${PROBE:-1}
LABEL=${LABEL:-smoke}
CPUS=${CPUS:-4}
OUT=$WT/scratchpad/logs/tier1/$LABEL
mkdir -p "$OUT"
docker run --rm --cpus "$CPUS" --security-opt seccomp=unconfined --ulimit memlock=134217728:134217728 \
  -e RUNS="$RUNS" -e ENGINES="$ENGINES" -e ARMS="$ARMS" -e PROBE="$PROBE" -e LABEL="$LABEL" \
  -e WS583_CONNS -e WS583_ECHO_FRAMES -e WS583_BP -e WS583_POSTPAUSE_BYTES -e WS583_CLIENT_RCVBUF -e WS583_CELLS \
  -v "$WT":/src -w /src -v /Users/fuming/go/pkg/mod:/go/pkg/mod -v gocache484:/root/.cache/go-build \
  golang:1.27 bash -c '
set -u
OUT=/src/scratchpad/logs/tier1/$LABEL
uname -r > "$OUT/env.txt"; nproc >> "$OUT/env.txt"; ulimit -l >> "$OUT/env.txt"; cat /proc/sys/net/ipv4/tcp_fin_timeout >> "$OUT/env.txt"
go test -c -tags celeris_closeprobe -o /tmp/ws583.test ./middleware/websocket/ > "$OUT/build.log" 2>&1 || { echo BUILD-FAILED; cat "$OUT/build.log"; exit 1; }
for run in $(seq 1 "$RUNS"); do
  for eng in $ENGINES; do
    for arm in $ARMS; do
      skip=0; [ "$arm" = off ] && skip=1
      log="$OUT/${eng}_${arm}_run${run}.log"
      start=$(date +%s)
      CELERIS_DEBUG_CLOSE_PROBE=$PROBE CELERIS_DEBUG_SKIP_CLOSE_DRAIN=$skip \
        /tmp/ws583.test -test.run "^TestServerInitiatedCloseDrainFINvsRST\$/^${eng}\$" -test.v -test.count=1 -test.timeout 15m > "$log" 2>&1
      rc=$?
      echo "RUN-EXIT=$rc elapsed=$(( $(date +%s) - start ))s" >> "$log"
      echo "$eng $arm run$run exit=$rc elapsed=$(( $(date +%s) - start ))s"
    done
  done
done
'
