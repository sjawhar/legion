#!/usr/bin/env bash
# The pmdoc differential: one corpus read at two revisions, each rendering read back by Go and by
# the browser editor's engine, and the head judged against the base (differential_test.go).
#
#   bash differential.sh --base <rev> [--head <rev>] [--corpus <file.jsonl|dir>]... [--out <dir>]
#
# --base and --head are git revisions of this repository (a jj commit id is one); --head defaults
# to the working tree. The in-repo corpus (testdata/corpus/*.md and testdata/corpus-display/*.md)
# is always read; each --corpus adds a JSONL file of {"id", "md"} lines, or every *.jsonl in a
# directory, ids prefixed with the file's name. It prints the gate's counts - regressions,
# misreads, churn and panics, each on its own line - and exits non-zero when any is above zero;
# every flagged document is in <out>/report.jsonl.
#
# Two of its gates are not base-against-head: the cross-reader gates, which hold Go's display read
# (ReadForDisplay) against the browser's engines over the head alone. The display read does not
# exist at every revision, and this script copies only differential_test.go into the base
# revision, so the base has nothing to compare; and the question those two gates answer is not
# whether the head moved but whether Go and the browser read one text the same way, which is what
# the display read is for. The read-only engine reads what DisplayLatest must read
# (engine-readonly.jsonl), and the editing engine what DisplayVersion must.
#
# Needs go, bun and jq, and `bun install` run in the repository.
set -euo pipefail

gen=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
pmdoc=$(dirname "$gen")
repo=$(git -C "$gen" rev-parse --show-toplevel)
base="" head="" out=""
corpora=()
while [ $# -gt 0 ]; do
  case $1 in
    --base) base=$2; shift 2 ;;
    --head) head=$2; shift 2 ;;
    --corpus) corpora+=("$2"); shift 2 ;;
    --out) out=$2; shift 2 ;;
    *) echo "differential.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$base" ] || { echo "differential.sh: --base <rev> is required" >&2; exit 2; }
out=${out:-$(mktemp -d)}
mkdir -p "$out"

# The corpus: the in-repo documents, the display read's own among them, then each given file,
# every line with a unique id.
corpus=$out/corpus.jsonl
: > "$corpus"
for directory in corpus corpus-display; do
  for file in "$pmdoc/testdata/$directory"/*.md; do
    jq -Rsc --arg id "$directory:$(basename "$file" .md)" '{id: $id, md: .}' "$file" >> "$corpus"
  done
done
for path in "${corpora[@]}"; do
  if [ -d "$path" ]; then files=("$path"/*.jsonl); else files=("$path"); fi
  for file in "${files[@]}"; do
    jq -c --arg file "$(basename "$file" .jsonl)" '{id: ($file + ":" + (.id | tostring)), md: .md}' "$file" >> "$corpus"
  done
done
echo "corpus: $(wc -l < "$corpus") documents in $out" >&2

# One test binary per revision, each built with the head's copy of the harness.
build() { # <name> <rev or empty for the working tree>
  local tree=$pmdoc
  if [ -n "$2" ]; then
    local root=$out/$1-tree
    rm -rf "$root" && mkdir -p "$root"
    git -C "$repo" archive "$2" packages/envoy | tar -x -C "$root"
    tree=$root/packages/envoy/internal/dispatch/pmdoc
    cp "$pmdoc/differential_test.go" "$tree/"
  fi
  (cd "$tree" && GOWORK=off go test -c -o "$out/$1.test" .)
}
build base "$base"
build head "$head"

# waitAll waits for every pid it is given and fails when any of them did.
waitAll() {
  local failed=0
  for pid in "$@"; do wait "$pid" || failed=1; done
  return "$failed"
}

# engineRead <in> <out> <field>...: the engine's reading of each field, over one shard of the
# lines per process, joined back in order. Its own messages go to <out>.log.
shards=${PMDOC_DIFF_SHARDS:-$(( $(nproc) / 4 > 0 ? $(nproc) / 4 : 1 ))}
engineRead() {
  local in=$1 result=$2
  shift 2
  split -n "l/$shards" -d -a 3 "$in" "$result.in."
  local pids=()
  for part in "$result".in.*; do
    bun "$gen/differential.ts" "$part" "${part/.in./.out.}" "$@" 2>> "$result.log" &
    pids+=($!)
  done
  waitAll "${pids[@]}" || { tail -n 20 "$result.log" >&2; return 1; }
  cat "$result".out.* > "$result"
  rm -f "$result".in.* "$result".out.*
}

engineRead "$corpus" "$out/engine.jsonl" md
# The third pass: the same corpus through the read-only engine, which is what a reader is shown
# and what the head's display read is held to (the cross-reader gate, above).
engineRead "$corpus" "$out/engine-readonly.jsonl" md --read-only
pids=()
for side in base head; do
  PMDOC_DIFF_CORPUS=$corpus PMDOC_DIFF_ENGINE=$out/engine.jsonl PMDOC_DIFF_OUT=$out/$side.jsonl \
    "$out/$side.test" -test.run '^TestDifferentialDump$' -test.count=1 -test.timeout=0 > "$out/$side.log" 2>&1 &
  pids+=($!)
done
waitAll "${pids[@]}" || { tail -n 20 "$out"/base.log "$out"/head.log >&2; exit 1; }
pids=()
for side in base head; do
  engineRead "$out/$side.jsonl" "$out/$side-engine.jsonl" render engine_render &
  pids+=($!)
done
waitAll "${pids[@]}"
PMDOC_DIFF_ENGINE=$out/engine.jsonl PMDOC_DIFF_BASE=$out/base.jsonl PMDOC_DIFF_HEAD=$out/head.jsonl \
  PMDOC_DIFF_BASE_ENGINE=$out/base-engine.jsonl PMDOC_DIFF_HEAD_ENGINE=$out/head-engine.jsonl \
  PMDOC_DIFF_ENGINE_READONLY=$out/engine-readonly.jsonl \
  PMDOC_DIFF_REPORT=$out/report.jsonl \
  "$out/head.test" -test.run '^TestDifferentialCompare$' -test.count=1 -test.timeout=0
