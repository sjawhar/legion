#!/usr/bin/env bash
# probe2.sh <tag> <head|base|prev>: probe.sh plus the engine's reading of Go's own rendering (engReadsGo).
set -euo pipefail
T=/legion/scratch/tmp/infra531-test
W=/legion/workspaces/sjawhar/legion/infra-531
tag=$1 side=$2
jq -Rc '{id: input_line_number, md: ((. | gsub("⏎"; "\n")) + "\n")}' "$T/$tag.txt" > "$T/$tag.jsonl"
(cd "$W/packages/envoy/internal/dispatch/pmdoc/gen" && bun differential.ts "$T/$tag.jsonl" "$T/$tag.eng.jsonl" md)
case $side in
  head) dir=$W/packages/envoy/internal/dispatch/pmdoc env="" ;;
  base) dir=$T/base1767/packages/envoy/internal/dispatch/pmdoc env="GOWORK=off" ;;
  prev) dir=$T/prev3ff/packages/envoy/internal/dispatch/pmdoc env="GOWORK=off" ;;
esac
cp "$T/zz_tester_probe_test.go" "$dir/"
trap 'rm -f "$dir/zz_tester_probe_test.go"' EXIT
(cd "$dir" && env $env PROBE_IN="$T/$tag.jsonl" PROBE_ENGINE="$T/$tag.eng.jsonl" PROBE_RENDERS_OUT="$T/$tag.$side.renders.jsonl" go test -count=1 -run TestZZTesterProbe . >/dev/null)
(cd "$W/packages/envoy/internal/dispatch/pmdoc/gen" && bun differential.ts "$T/$tag.$side.renders.jsonl" "$T/$tag.$side.renders.eng.jsonl" md)
(cd "$dir" && env $env PROBE_IN="$T/$tag.jsonl" PROBE_ENGINE="$T/$tag.eng.jsonl" PROBE_RENDERS_ENGINE="$T/$tag.$side.renders.eng.jsonl" PROBE_ONLY_BAD="${ONLY_BAD:-}" go test -count=1 -v -run TestZZTesterProbe . | grep -v '^=== RUN\|^--- PASS\|^PASS\|^ok')
