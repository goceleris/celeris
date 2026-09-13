#!/bin/bash
# Tier 0 (celeris#583): kernel FIN/RST truth table on loopback TCP.
# Run 1: default sysctls (tcp_fin_timeout=60), plain + -race.
# Run 2: --sysctl net.ipv4.tcp_fin_timeout=120 so the post-close-data reset
#        stays a full-socket FIN_WAIT2 reset and TCPAbortOnData is countable.
WT=/Users/fuming/Documents/github/celeris/celeris/.claude/worktrees/wf-583
mkdir -p "$WT/scratchpad/logs/tier0"
TAG=${1:-2}
LOG=$WT/scratchpad/logs/tier0/tier0_run${TAG}_default.log
docker run --rm --cpus 4 --security-opt seccomp=unconfined --ulimit memlock=134217728:134217728 \
  -v "$WT":/src -w /src -v /Users/fuming/go/pkg/mod:/go/pkg/mod -v gocache484:/root/.cache/go-build \
  golang:1.27 sh -c 'uname -r; cat /proc/sys/net/ipv4/tcp_fin_timeout; go test -count=1 -v -run TestDrainRecvBufferTCPTruthTable ./internal/sockopts/ 2>&1; echo TIER0-PLAIN-EXIT=$?; go test -count=1 -race -v -run TestDrainRecvBufferTCPTruthTable ./internal/sockopts/ 2>&1; echo TIER0-RACE-EXIT=$?' > "$LOG" 2>&1
echo docker-rc=$? >> "$LOG"
LOG=$WT/scratchpad/logs/tier0/tier0_run${TAG}_fin120.log
docker run --rm --cpus 4 --security-opt seccomp=unconfined --ulimit memlock=134217728:134217728 \
  --sysctl net.ipv4.tcp_fin_timeout=120 \
  -v "$WT":/src -w /src -v /Users/fuming/go/pkg/mod:/go/pkg/mod -v gocache484:/root/.cache/go-build \
  golang:1.27 sh -c 'uname -r; cat /proc/sys/net/ipv4/tcp_fin_timeout; go test -count=1 -v -run TestDrainRecvBufferTCPTruthTable ./internal/sockopts/ 2>&1; echo TIER0-PLAIN-EXIT=$?' > "$LOG" 2>&1
echo docker-rc=$? >> "$LOG"
