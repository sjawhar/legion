#!/usr/bin/env bash
# direct-run.sh SIDE NAME: one run of step-10x (600 s window) on SIDE (before|after) with directpub
# beside it: Dispatch-shaped issue events published straight to the stack's NATS (JetStream) at
# 40/s for the whole window, so the delivery leg sees 40 events/s (x5 watchers) although Dispatch
# commits far less on this box. A labelled supplement to the main runs, not one of them.
set -euo pipefail
L=/legion/scratch/tmp/infra594/load
side=$1 name=$2
[ "$side" = before ] && other=after || other=before
[ "$side" = before ] && nats=nats://127.0.0.1:14222 || nats=nats://127.0.0.1:24222
out=$L/runs/$name
(source "$L/env-$other.sh" && "$L/$other/packages/envoy/loadtest/stack.sh" stop) || true
sleep 20
spy=""
if [ "$side" = before ]; then
  "$L/tools/natsspy/natsspy" -nats "$nats" -mon http://127.0.0.1:18222 >"$L/runs/$name.natsspy.jsonl" 2>"$L/runs/$name.natsspy.err" &
  spy=$!
fi
(source "$L/env-$side.sh" && cd "$L/$side" && packages/envoy/loadtest/run-step.sh step-10x --events 40 --sessions 1000 --requests 600 --duration 600s --out "$out") >"$L/runs/$name.log" 2>&1 &
step=$!
until jq -e '.window_start | type == "string" and (startswith("0001") | not)' "$out/meta.json" >/dev/null 2>&1; do
  kill -0 "$step" 2>/dev/null || { echo "direct-run.sh: the step ended before its window opened" >&2; exit 1; }
  sleep 1
done
ws=$(jq -r .window_start "$out/meta.json")
"$L/tools/directpub/directpub" -nats "$nats" -seed "$L/root-$side/seed.json" -rate 40 -duration 600s -start "$ws" >"$L/runs/$name.directpub.json"
rc=0
wait "$step" || rc=$?
[ -n "$spy" ] && kill "$spy" 2>/dev/null || true
echo "direct-run.sh: step exit $rc" >&2
cat "$L/runs/$name.directpub.json"
