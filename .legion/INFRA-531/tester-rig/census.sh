#!/usr/bin/env bash
# census.sh <versions.jsonl> <outdir>: main's tree per version, then the head's classification.
set -euo pipefail
T=/legion/scratch/tmp/infra531-test
W=/legion/workspaces/sjawhar/legion/infra-531
in=$1 out=$2
mkdir -p "$out"
B=$T/basenew/packages/envoy/internal/dispatch/pmdoc
H=$W/packages/envoy/internal/dispatch/pmdoc
cp "$T/zz_census_test.go" "$B/" && cp "$T/zz_census_test.go" "$H/"
trap 'rm -f "$B/zz_census_test.go" "$H/zz_census_test.go"' EXIT
(cd "$B" && GOWORK=off CENSUS_IN="$in" CENSUS_OUT="$out/base.jsonl" go test -count=1 -timeout 0 -run TestZZCensusDump . >"$out/base.log" 2>&1)
(cd "$H" && CENSUS_IN="$in" CENSUS_BASE="$out/base.jsonl" CENSUS_OUT="$out/compare.jsonl" go test -count=1 -timeout 0 -run TestZZCensusCompare . >"$out/head.log" 2>&1)
jq -s '{
  versions: length,
  documents_latest: (map(select(.latest)) | length),
  reading_changes: (map(select(.readingChanges)) | length),
  first_write_changes_links: (map(select(.firstWriteChangesLinks)) | length),
  first_write_changes_links_latest: (map(select(.firstWriteChangesLinks and .latest)) | length),
  first_write_changes_links_approved: (map(select(.firstWriteChangesLinks and .approved)) | length),
  bytes_change: (map(select(.bytesChange)) | length),
  errors: (map(select(.baseErr or .headErr or .renderErr or .backErr or .treeErr or .panic)) | length)
}' "$out/compare.jsonl"
