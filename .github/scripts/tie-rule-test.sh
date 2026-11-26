#!/usr/bin/env bash
# Tests of the per-scenario tie rule of a composed cell (tie_rule.py) in the two README scripts.
set -euo pipefail
exec python3 "$(dirname "$0")/tie_rule_test.py"
