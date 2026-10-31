#!/usr/bin/env bash
# restart-run.sh NAME: one AFTER run of step-10x (300 s window) during which the owning listener is
# killed (SIGKILL) 120 s into the window, kept down 30 s, and started again (restart-listener.sh).
# 60 s into the window, backlogpub publishes one envelope to each session's agent subject, so every
# session has answered a frame done and holds a resume point, as a session running for a while
# does. Starting 2 s after the kill, backlogpub publishes 3 envelopes to each of the 1,000
# sessions' agent subjects over 25 s, so every session has a backlog when the listener comes back.
# The step itself fails (the sampler sees the listener's pid exit), but it still writes
# summary.json and relay-frames.csv, which analyze_restart.py reads.
# WARM=0 skips the 60 s step: no session has answered a frame done, so none holds a resume point
# (the 'after-restart' run's procedure).
set -euo pipefail
L=/legion/scratch/tmp/infra594/load
name=${1:-after-restart}
warm=${WARM:-1}
out=$L/runs/$name
(source "$L/env-before.sh" && "$L/before/packages/envoy/loadtest/stack.sh" stop) || true
sleep 20
(source "$L/env-after.sh" && cd "$L/after" && packages/envoy/loadtest/run-step.sh step-10x --events 40 --sessions 1000 --requests "${REQUESTS:-600}" --duration 300s --out "$out") >"$L/runs/$name.log" 2>&1 &
step=$!
until jq -e '.window_start | type == "string" and (startswith("0001") | not)' "$out/meta.json" >/dev/null 2>&1; do
  kill -0 "$step" 2>/dev/null || { echo "restart-run.sh: the step ended before its window opened" >&2; exit 1; }
  sleep 1
done
ws=$(jq -r .window_start "$out/meta.json")
warm_at=$(( $(date -d "$ws" +%s) + 60 ))
kill_at=$(( $(date -d "$ws" +%s) + 120 ))
echo "restart-run.sh: window starts $ws; kill at $(date -u -d "@$kill_at" +%FT%TZ)" >&2
sleep $(( warm_at - $(date +%s) ))
if [ "$warm" != 0 ]; then
  "$L/tools/backlogpub/backlogpub" -nats nats://127.0.0.1:24222 -sessions 1000 -per 1 -over 10s -id-base 910000000 >"$L/runs/$name.warm.json"
fi
sleep $(( kill_at - $(date +%s) ))
"$L/restart-listener.sh" "$L/root-after" 30 "$L/runs/$name.restart.json" &
rs=$!
sleep 2
"$L/tools/backlogpub/backlogpub" -nats nats://127.0.0.1:24222 -sessions 1000 -per 3 -over 25s >"$L/runs/$name.backlog.json"
wait "$rs"
rc=0
wait "$step" || rc=$?
echo "restart-run.sh: step exit $rc (a listener exit fails the step by design)" >&2
cat "$L/runs/$name.restart.json" "$L/runs/$name.backlog.json"
