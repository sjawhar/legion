#!/usr/bin/env bash
# The source a proof's binaries were built from, printed by every driver that builds them, so a
# run's head is on its own log rather than only in whatever comment reported the run. jj is the
# repository's own tool and says whether the working copy was clean; a checkout without it falls
# back to git's HEAD, which is what CI has.

# built_revision ROOT prints the revision and, under jj, whether the working copy carried changes.
built_revision() {
  local root=$1 working parent
  if command -v jj >/dev/null && jj -R "$root" root >/dev/null 2>&1; then
    # Each read is captured before anything is printed: a jj that fails here must stop the run,
    # as it did when this expression lived in the Stage 2 driver, rather than leave a revision
    # line with a hole in it and a zero status behind printf's.
    working=$(jj -R "$root" log -r @ --no-graph \
      -T 'commit_id ++ if(empty, " (working copy: no changes)", " (working copy has changes)")') || return
    parent=$(jj -R "$root" log -r @- --no-graph -T 'commit_id') || return
    printf '%s on %s\n' "$working" "$parent"
    return
  fi
  git -C "$root" rev-parse HEAD
}

# built_from ROOT BINARY... prints one line naming the revision and each binary's sha256, in the
# caller's `note` when it has one and plainly otherwise.
built_from() {
  local root=$1 revision line binary
  shift
  revision=$(built_revision "$root") || return
  line="built from $revision"
  for binary in "$@"; do
    line="$line; $(basename "$binary") sha256 $(sha256sum "$binary" | cut -d' ' -f1)"
  done
  if declare -F note >/dev/null; then
    note "$line"
  else
    printf '   %s\n' "$line"
  fi
}
