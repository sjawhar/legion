#!/bin/bash
# list conflicting merges on target files
for p in $(cat ../prs.txt); do
  for m in $(git rev-list --merges refs/pr/$p ^upstream/main); do
    p1=$(git rev-parse $m^1); p2=$(git rev-parse $m^2)
    files=$(git merge-tree --write-tree --name-only $p1 $p2 2>/dev/null | tail -n +2 | sed '/^$/,$d')
    for f in packages/envoy/AGENTS.md packages/envoy/cmd/dispatch/README.md; do
      if echo "$files" | grep -qx "$f"; then echo "$p $m $p1 $p2 $f $(git log -1 --format=%cs $m)"; fi
    done
  done
done
