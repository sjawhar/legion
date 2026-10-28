#!/usr/bin/env bash
# probe.sh <tag> <head|base>: reads $T/<tag>.txt (one markdown per line, ⏎ for a line break),
# runs the engine and the Go probe, prints the probe output.
set -euo pipefail
T=/legion/scratch/tmp/infra531-test
W=/legion/workspaces/sjawhar/legion/infra-531
tag=$1 side=$2
if [ ! -f "$T/$tag.eng.jsonl" ] || [ "$T/$tag.txt" -nt "$T/$tag.eng.jsonl" ]; then
  jq -Rc '{id: input_line_number, md: ((. | gsub("⏎"; "\n")) + "\n")}' "$T/$tag.txt" > "$T/$tag.jsonl"
  (cd "$W/packages/envoy/internal/dispatch/pmdoc/gen" && bun differential.ts "$T/$tag.jsonl" "$T/$tag.eng.jsonl" md)
fi
if [ "$side" = head ]; then
  dir=$W/packages/envoy/internal/dispatch/pmdoc env=""
else
  dir=$T/base690/packages/envoy/internal/dispatch/pmdoc env="GOWORK=off"
fi
cp "$T/zz_tester_probe_test.go" "$dir/"
trap 'rm -f "$dir/zz_tester_probe_test.go"' EXIT
(cd "$dir" && env $env PROBE_IN="$T/$tag.jsonl" PROBE_ENGINE="$T/$tag.eng.jsonl" PROBE_ONLY_BAD="${ONLY_BAD:-}" go test -count=1 -v -run TestZZTesterProbe . | grep -v '^=== RUN\|^--- PASS\|^PASS\|^ok')
