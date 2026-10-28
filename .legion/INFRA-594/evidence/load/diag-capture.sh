#!/usr/bin/env bash
# diag-capture.sh OUTDIR: wait for the restarted listener's diagnostic pprof (127.0.0.1:15062) to
# answer after its kill, then capture a 20 s CPU profile, block and mutex profiles, goroutine dumps
# every second and NATS connz every 0.5 s for 20 s.
set -uo pipefail
out=$1
mkdir -p "$out"
# wait for the old listener to go away first (its pprof answers too)
old=$(cat /legion/scratch/tmp/infra594/load/root-after/run/listener.pid)
while [ -e "/proc/$old" ] && ! grep -q '^State:.*Z' "/proc/$old/status" 2>/dev/null; do sleep 0.2; done
until curl -fsS --max-time 1 http://127.0.0.1:15062/debug/pprof/ >/dev/null 2>&1; do sleep 0.05; done
date +%s%N >"$out/pprof_up_ns"
curl -fsS "http://127.0.0.1:15062/debug/pprof/profile?seconds=20" -o "$out/cpu.pprof" &
curl -fsS "http://127.0.0.1:15063/debug/pprof/profile?seconds=20" -o "$out/nats-cpu.pprof" &
for i in $(seq 0 39); do
  curl -fsS --max-time 2 "http://127.0.0.1:28222/connz?sort=pending&limit=8" -o "$out/connz.$i.json"
  if (( i % 2 == 0 )); then
    curl -fsS --max-time 3 "http://127.0.0.1:15062/debug/pprof/goroutine?debug=1" -o "$out/gor.$((i/2)).txt"
  fi
  sleep 0.5
done
curl -fsS "http://127.0.0.1:15062/debug/pprof/block" -o "$out/block.pprof"
curl -fsS "http://127.0.0.1:15062/debug/pprof/mutex" -o "$out/mutex.pprof"
wait
