#!/usr/bin/env bash
# diag-run.sh NAME: restart-run.sh NAME (WARM and LOADTEST_NO_RESUME pass through) with
# diag-capture.sh capturing the restarted listener's burst into runs/NAME.diag/.
set -uo pipefail
L=/legion/scratch/tmp/infra594/load
name=$1
"$L/restart-run.sh" "$name" >"$L/runs/$name.driver.log" 2>&1 &
run=$!
out=$L/runs/$name
until jq -e '.window_start | type == "string" and (startswith("0001") | not)' "$out/meta.json" >/dev/null 2>&1; do
  kill -0 "$run" 2>/dev/null || { echo "diag-run.sh: the run ended early" >&2; exit 1; }
  sleep 1
done
ws=$(jq -r .window_start "$out/meta.json")
sleep $(( $(date -d "$ws" +%s) + 110 - $(date +%s) ))
"$L/diag-capture.sh" "$L/runs/$name.diag" &
wait "$run"
wait
python3 "$L/analyze_restart.py" "$name"
