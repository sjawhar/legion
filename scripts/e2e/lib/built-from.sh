#!/usr/bin/env bash
# Prints what a stage proof ran: the source revision its binaries were built from, what a changed
# working copy held, and each binary's sha256, so the run can be tied to a commit after its scratch
# directory is gone. One line each on stdout, for the caller to print with its own `note`.
#
#   bash scripts/e2e/lib/built-from.sh <root> [<binary>...]
#
# The source is the working copy's commit and its parents under jj, or HEAD under git (CI's
# checkout). A working copy with changes also prints the sha256 of its diff and its diff --stat, so
# a run on one - a negative control built from a base with the new script copied in - says what it
# ran rather than only that something changed.
set -euo pipefail

root=${1:?usage: built-from.sh <root> [<binary>...]}
shift

if command -v jj >/dev/null && jj -R "$root" root >/dev/null 2>&1; then
  changed=$(jj -R "$root" log -r @ --no-graph -T 'if(empty, "", "yes")')
  echo "source: $(jj -R "$root" log -r @ --no-graph -T 'commit_id')${changed:+ (working copy has changes)} on $(jj -R "$root" log -r @ --no-graph -T 'parents.map(|p| p.commit_id()).join(" and ")')"
  if [ -n "$changed" ]; then
    echo "working-copy changes, sha256 $(jj -R "$root" diff --git | sha256sum | cut -d' ' -f1):"
    jj -R "$root" diff --stat | sed 's/^/  /'
  fi
else
  echo "source: $(git -C "$root" rev-parse HEAD)"
  if [ -n "$(git -C "$root" status --porcelain)" ]; then
    echo "working-tree changes, sha256 $(git -C "$root" diff HEAD | sha256sum | cut -d' ' -f1):"
    git -C "$root" diff HEAD --stat | sed 's/^/  /'
  fi
fi
for binary in "$@"; do
  echo "$(basename "$binary"): sha256 $(sha256sum "$binary" | cut -d' ' -f1)"
done
