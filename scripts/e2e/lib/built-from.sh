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
#
# Each binary is a Go binary, and the toolchain stamped it at build time with the commit its
# checkout's git HEAD named (jj keeps that at the working copy's first parent) and whether the tree
# differed from it (vcs.revision, vcs.modified). A binary whose stamp names a commit other than the
# source's, or whose modified flag disagrees with the tree now, was not built from this source, and
# the helper fails naming both. The stamp does not hash a changed tree, so an edit between the build
# and this call to a tree that was already changed stays unseen: callers call it right after the
# build.
set -euo pipefail

root=${1:?usage: built-from.sh <root> [<binary>...]}
shift

if command -v jj >/dev/null && jj -R "$root" root >/dev/null 2>&1; then
  vcs=jj
  changed=$(jj -R "$root" log -r @ --no-graph -T 'if(empty, "", "yes")')
  parents=$(jj -R "$root" log -r @ --no-graph -T 'parents.map(|p| p.commit_id()).join(" ")')
  echo "source: $(jj -R "$root" log -r @ --no-graph -T 'commit_id')${changed:+ (working copy has changes)} on ${parents// / and }"
  if [ -n "$changed" ]; then
    echo "working-copy changes, sha256 $(jj -R "$root" diff --git | sha256sum | cut -d' ' -f1):"
    jj -R "$root" diff --stat | sed 's/^/  /'
  fi
else
  vcs=git
  parents=$(git -C "$root" rev-parse HEAD)
  echo "source: $parents"
  if [ -n "$(git -C "$root" status --porcelain)" ]; then
    echo "working-tree changes, sha256 $(git -C "$root" diff HEAD | sha256sum | cut -d' ' -f1):"
    git -C "$root" diff HEAD --stat | sed 's/^/  /'
  fi
fi
refuse() {
  echo "built-from: $*" >&2
  exit 1
}
for binary in "$@"; do
  info=$(go version -m "$binary" 2>&1) || refuse "$binary carries no Go build information: $info"
  revision=$(awk '$2 ~ /^vcs\.revision=/ { sub(/^vcs\.revision=/, "", $2); print $2 }' <<<"$info")
  modified=$(awk '$2 ~ /^vcs\.modified=/ { sub(/^vcs\.modified=/, "", $2); print $2 }' <<<"$info")
  [ -n "$revision" ] && [ -n "$modified" ] || refuse "$binary carries no vcs stamp (built with -buildvcs=false, or outside a checkout)"
  case " $parents " in *" $revision "*) ;; *) refuse "$binary was built from $revision, not from the source's $parents" ;; esac
  if [ "$vcs" = jj ]; then
    differs=$([ -n "$(jj -R "$root" diff --from "$revision" --to @ --name-only)" ] && echo true || echo false)
  else
    differs=$([ -n "$(git -C "$root" status --porcelain)" ] && echo true || echo false)
  fi
  [ "$modified" = "$differs" ] || refuse "$binary's stamp says vcs.modified=$modified at build time, and the tree now differs from $revision: $differs"
  echo "$(basename "$binary"): sha256 $(sha256sum "$binary" | cut -d' ' -f1), stamped $revision modified=$modified"
done
