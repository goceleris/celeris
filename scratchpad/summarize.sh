#!/bin/bash
# Produce the compact summaries that get committed next to the runner scripts.
L=/Users/fuming/Documents/github/celeris/celeris/.claude/worktrees/wf-583/scratchpad/logs
S=/Users/fuming/Documents/github/celeris/celeris/.claude/worktrees/wf-583/scratchpad
{
  for f in "$L/tier0/tier0_run2_default.log" "$L/tier0/tier0_run2_fin120.log"; do
    echo "== $(basename "$f") =="
    grep -h "TIER0-ENV\|TIER0-CELL\|EXIT=\|^ok\|^FAIL" "$f" | sed 's/^ *drain_recv_tcp_linux_test.go:[0-9]*: //'
  done
} > "$L/tier0_summary.txt"
python3 "$S/tally_tier1.py" "$L/tier1/main" > "$L/tier1_main_tally.txt"
python3 "$S/tally_tier1.py" "$L/tier1/probeoff" > "$L/tier1_probeoff_tally.txt"
{
  cat "$L/tier1_control_runner.txt"
  echo "== CLOSE-PROBE records with inq_before>0 =="
  grep -h "^CLOSE-PROBE" "$L"/tier1/control_oracles/*.log | grep -v "inq_before=0 "
  echo "== oracle summary lines =="
  grep -h "conns=.*closedOK=" "$L"/tier1/control_oracles/*.log | sed 's/^ *[a-z_]*\.go:[0-9]*: //'
} > "$L/tier1_control_summary.txt"
cat "$L/env_main.txt" 2>/dev/null
cp "$L/tier1/main/env.txt" "$L/tier1_env.txt"
wc -l "$L/tier0_summary.txt" "$L/tier1_main_tally.txt" "$L/tier1_probeoff_tally.txt" "$L/tier1_control_summary.txt" "$L/tier1_env.txt"
