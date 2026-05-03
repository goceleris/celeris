#!/usr/bin/env bash
# Stress matrix for the distributed cluster bench. Loops through every
# (engine × protocol × target) combo and runs `mage clusterDistributedBench`
# with the right loadgen flags for each, capturing throughput + CPU on
# both ends.
#
# Engines × Protocols × Targets:
#   { iouring, epoll, adaptive } × { h1, h2c-noupg, h2c+upg, auto+upg } × { msa2-server, msr1 }
#   = 24 runs, 60 s each → ~25-30 min total.
#
# After each run, parses loadgen.json (RPS, latency tail) and cpu.log
# (peak %CPU during the bench) on both bench target and msa2-client
# and prints a single-line summary. Aggregates into a table at the end.
#
# Output dir: results/<ts>-stress-matrix/
#
# Env knobs:
#   STRESS_DURATION   default 60s
#   STRESS_WARMUP     default 5s
#   STRESS_CONNS      default 256
#   STRESS_RUN_FILTER (regex) default '' (all 24 cells)
set -euo pipefail

cd "$(dirname "$0")/../.."   # repo root

# sysstat (mpstat) is the one tool the matrix needs that isn't already on
# pristine cluster nodes. Install once before the loop, uninstall after.
# Failures here abort the matrix — better to fail fast than report 0.0% CPU.
install_sysstat() {
  echo "Installing sysstat on all cluster nodes..."
  (cd ansible && ansible cluster -i inventory.yml -m apt \
    -a "name=sysstat state=present update_cache=true" -b -o)
}
remove_sysstat() {
  echo "Removing sysstat from all cluster nodes..."
  (cd ansible && ansible cluster -i inventory.yml -m apt \
    -a "name=sysstat state=absent purge=true autoremove=true" -b -o) || true
}
trap remove_sysstat EXIT
install_sysstat

DURATION="${STRESS_DURATION:-60s}"
WARMUP="${STRESS_WARMUP:-5s}"
CONNS="${STRESS_CONNS:-256}"
FILTER="${STRESS_RUN_FILTER:-}"
TS=$(date -u +%Y%m%d-%H%M%S)
RESULTS_ROOT="results/${TS}-stress-matrix"
mkdir -p "$RESULTS_ROOT"
SUMMARY="$RESULTS_ROOT/summary.tsv"

# header
{
  printf "target\tengine\tprotocol\tasync\tserver_name\trps\tp50_us\tp99_us\tp99_99_us\terrors\ttarget_cpu_avg\ttarget_cpu_peak\tloadgen_cpu_avg\tloadgen_cpu_peak\n"
} > "$SUMMARY"

# Compute per-CPU avg/peak from `mpstat -P ALL 1 N` output. Aggregate
# %usr+%sys+%irq+%soft as "active" (idle and iowait are excluded). Returns
# two numbers: "all-CPU mean active%" and "peak active% across any single CPU
# during the run".
parse_mpstat() {
  local logfile="$1"
  if [ ! -f "$logfile" ]; then
    echo "0.0 0.0"
    return
  fi
  python3 - "$logfile" <<'PY'
import sys, re
path = sys.argv[1]
records = []  # one (cpu, active%) per ALL-line per second
peaks = {}    # cpu -> max active during run
all_active = []  # one mean per second for the "all" row
with open(path) as f:
    for line in f:
        # mpstat uses locale-aware time prefix; we look for lines that have
        # CPU column (numeric or "all") and at least 8 numeric fields.
        parts = line.split()
        if len(parts) < 11:
            continue
        # detect the CPU column (typically index 2 after "Average:" or HH:MM:SS AM/PM markers)
        # Standard mpstat format: time CPU %usr %nice %sys %iowait %irq %soft %steal %guest %gnice %idle
        try:
            # find the CPU token: "all" or an integer
            for i, tok in enumerate(parts):
                if tok == "all" or (tok.isdigit() and i >= 1):
                    cpu = tok
                    nums = parts[i+1:]
                    # mpstat columns after CPU:
                    #   %usr %nice %sys %iowait %irq %soft %steal %guest %gnice %idle
                    if len(nums) < 10: continue
                    usr  = float(nums[0])
                    sys_ = float(nums[2])
                    irq  = float(nums[4])
                    soft = float(nums[5])
                    active = usr + sys_ + irq + soft
                    if cpu == "all":
                        all_active.append(active)
                    else:
                        peaks[cpu] = max(peaks.get(cpu, 0.0), active)
                    break
        except (ValueError, IndexError):
            continue
mean_all = sum(all_active)/len(all_active) if all_active else 0.0
peak_any = max(peaks.values()) if peaks else 0.0
print(f"{mean_all:.1f} {peak_any:.1f}")
PY
}

# Returns rps p50_us p99_us p99_99_us errors  (whitespace-separated).
parse_loadgen_json() {
  local jsonfile="$1"
  if [ ! -s "$jsonfile" ]; then
    echo "0 0 0 0 0"
    return
  fi
  python3 - "$jsonfile" <<'PY'
import sys, json
with open(sys.argv[1]) as f:
    r = json.load(f)
rps = int(r.get("requests_per_sec", 0))
lat = r.get("latency", {})
p50    = int(lat.get("p50", 0)) // 1000      # ns → µs
p99    = int(lat.get("p99", 0)) // 1000
p99_99 = int(lat.get("p99_99", 0)) // 1000
errs = int(r.get("errors", 0))
print(f"{rps} {p50} {p99} {p99_99} {errs}")
PY
}

# Configs to test. Each row: <key> <server_name> <h2_flag> <h2c_upgrade_flag>
# where h2_flag/h2c_upgrade_flag are "true" or "false".
CONFIGS=(
  "h1            celeris-{ENG}-h1-async               false false"
  "h2c-noupg     celeris-{ENG}-h2c-noupg-async        true  false"
  "h2c+upg       celeris-{ENG}-h2c+upg-async          false true"
  "auto+upg      celeris-{ENG}-auto+upg-async         false true"
)
ENGINES=(iouring epoll adaptive)
TARGETS=(msa2-server msr1)

for target in "${TARGETS[@]}"; do
  for engine in "${ENGINES[@]}"; do
    for cfg in "${CONFIGS[@]}"; do
      proto=$(awk '{print $1}' <<<"$cfg")
      server_tmpl=$(awk '{print $2}' <<<"$cfg")
      h2=$(awk '{print $3}' <<<"$cfg")
      h2c_upg=$(awk '{print $4}' <<<"$cfg")
      server="${server_tmpl//\{ENG\}/$engine}"

      cell="$target/$engine/$proto"
      if [ -n "$FILTER" ] && ! [[ "$cell" =~ $FILTER ]]; then
        continue
      fi

      echo
      echo "================================================================"
      echo " [$cell]  server=$server  h2=$h2 h2c_upg=$h2c_upg"
      echo "================================================================"

      # The mage target stages binaries to a fresh /tmp/celeris-bench-stage-* and
      # creates results/<ts>-cluster/ for fetched files. We then rename that dir
      # into our matrix root so we keep one tidy tree.
      RUN_LOG="$RESULTS_ROOT/${target}_${engine}_${proto}.run.log"
      if PATH=/usr/local/go/bin:$HOME/go/bin:$PATH \
        CLUSTER_DIST_TARGET="$target" \
        CLUSTER_DIST_SERVER="$server" \
        CLUSTER_DIST_DURATION="$DURATION" \
        CLUSTER_DIST_WARMUP="$WARMUP" \
        CLUSTER_DIST_CONNS="$CONNS" \
        CLUSTER_DIST_H2="$h2" \
        CLUSTER_DIST_H2C_UPGRADE="$h2c_upg" \
        mage clusterDistributedBench >"$RUN_LOG" 2>&1
      then
        echo "  bench: ok"
      else
        rc=$?
        echo "  bench: FAILED rc=$rc — see $RUN_LOG"
        printf "%s\t%s\t%s\tasync\t%s\tFAIL\t-\t-\t-\t-\t-\t-\t-\t-\n" \
          "$target" "$engine" "$proto" "$server" >> "$SUMMARY"
        continue
      fi

      # Find the mage results dir from the run log (last line: "Results in /abs/path/results/...-cluster")
      MAGE_RESULTS=$(grep -oE 'Results in [^ ]+-cluster' "$RUN_LOG" | tail -1 | awk '{print $3}')
      if [ -z "$MAGE_RESULTS" ] || [ ! -d "$MAGE_RESULTS" ]; then
        echo "  WARN: could not locate mage results dir from $RUN_LOG"
        continue
      fi

      # Move into the matrix root with a stable name.
      DEST="$RESULTS_ROOT/${target}_${engine}_${proto}"
      mv "$MAGE_RESULTS" "$DEST"

      LOADGEN_JSON="$DEST/loadgen/loadgen.json"
      TARGET_CPU="$DEST/$target/cpu.log"
      LOADGEN_CPU="$DEST/loadgen/cpu.log"

      read -r RPS P50 P99 P99_99 ERRS < <(parse_loadgen_json "$LOADGEN_JSON")
      read -r TGT_AVG TGT_PEAK < <(parse_mpstat "$TARGET_CPU")
      read -r LG_AVG  LG_PEAK  < <(parse_mpstat "$LOADGEN_CPU")

      printf "  rps=%d p50=%dµs p99=%dµs p99.99=%dµs errs=%d  target_cpu=%s/%s loadgen_cpu=%s/%s\n" \
        "$RPS" "$P50" "$P99" "$P99_99" "$ERRS" "$TGT_AVG" "$TGT_PEAK" "$LG_AVG" "$LG_PEAK"

      printf "%s\t%s\t%s\tasync\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n" \
        "$target" "$engine" "$proto" "$server" \
        "$RPS" "$P50" "$P99" "$P99_99" "$ERRS" \
        "$TGT_AVG" "$TGT_PEAK" "$LG_AVG" "$LG_PEAK" >> "$SUMMARY"
    done
  done
done

echo
echo "================================================================"
echo " Stress matrix complete. Summary: $SUMMARY"
echo "================================================================"
column -t -s $'\t' < "$SUMMARY"
