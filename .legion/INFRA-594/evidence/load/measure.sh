#!/usr/bin/env bash
# measure.sh FIRST LAST: runs INFRA-589's step-10x (40 events/s, 1,000 sessions, 600 requests/s,
# 20 SSE streams; run-series.sh's row) with a 600 s window, alternating BEFORE and AFTER for runs
# FIRST..LAST. Each side runs on its own LOADTEST_ROOT with its own seed; the other side's stack is
# stopped during a run. BEFORE runs get natsspy beside them (consumer count and pull rate, which
# INFRA-589's harness does not take); AFTER runs take them in the harness.
set -euo pipefail
L=/legion/scratch/tmp/infra594/load
mkdir -p "$L/runs"
step=(step-10x --events 40 --sessions 1000 --requests 600 --duration 600s)

run_side() {
  local side=$1 n=$2 other
  [ "$side" = before ] && other=after || other=before
  (source "$L/env-$other.sh" && "$L/$other/packages/envoy/loadtest/stack.sh" stop) || true
  sleep 20
  local out=$L/runs/$side-r$n spy=""
  [ -e "$out" ] && { echo "measure.sh: $out exists; skipping" >&2; return 0; }
  echo "measure.sh: $(date -u +%FT%TZ) $side run $n -> $out" >&2
  if [ "$side" = before ]; then
    "$L/tools/natsspy/natsspy" -nats nats://127.0.0.1:14222 -mon http://127.0.0.1:18222 >"$L/runs/$side-r$n.natsspy.jsonl" 2>"$L/runs/$side-r$n.natsspy.err" &
    spy=$!
  fi
  local rc=0
  (source "$L/env-$side.sh" && cd "$L/$side" && packages/envoy/loadtest/run-step.sh "${step[@]}" --out "$out") >"$L/runs/$side-r$n.log" 2>&1 || rc=$?
  [ -n "$spy" ] && kill "$spy" 2>/dev/null || true
  echo "measure.sh: $(date -u +%FT%TZ) $side run $n exit $rc" >&2
}

for ((n = $1; n <= $2; n++)); do
  run_side before "$n"
  run_side after "$n"
done
echo "measure.sh: done" >&2
