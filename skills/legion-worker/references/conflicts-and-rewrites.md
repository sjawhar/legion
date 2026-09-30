# Conflicts, retargets, fingerprints, and rewriting pushed commits

Part of `skill://legion-worker`. Read it when GitHub reports the pull request `CONFLICTING`, the
controller asks you to resolve a conflict, the pull request is retargeted to a new base, you
compare two heads after such a merge, or you are about to rewrite a commit you already pushed.
Every path it cites is in sjawhar/legion.

## Reintegrating the base

- **Reintegrate the base only on a real conflict, except after a base retarget — and with a
  merge, never `jj rebase`.** Sami, 2026-09-11, verbatim:
  "Please don't do unnecessary rebases (i.e. unless there are merge conflicts). The CI queue is too long and slow."
  The implementer merges the base into the issue branch only when GitHub reports it `CONFLICTING`, the controller asks
  because of a conflict, or after the pull request is retargeted to a new base. Otherwise, never reintegrate the base to
  pick up `main` or refresh CI (a single failed CI job is re-run on its own: *A red CI job* in
  `skill://legion-worker`). A conflict-forced rebase that leaves the branch's diff unchanged is a
  confirmation, not a new round (see *The unchanged-diff check* below); that name is the event's,
  kept by the rules below and the learnings that cite it, and the operation it names is always
  the merge here. Before merging, record
  the fingerprint at the current tip; after pushing the merged branch, record it at the new
  tip; post one PR comment (Legion footer):
  `rebase <old-tip-sha> → <new-tip-sha>; fingerprint <before> → <after>; unchanged|changed`.
  Every issue workspace is a `jj workspace` of the same shared repository and operation log, and
  jj always rebases every descendant of any commit it rewrites — a revset naming the root of your
  own chain and rewriting it in place also rewrites whatever another tree has stacked on that root,
  whichever selector chose it (`-s`, `-b`, and `-r` all rewrite descendants; `-r` only re-parents
  them to fill the hole, which is worse). This is what happened in LEGION-118: one issue's own
  conflict step moved a second issue's twelve commits and its bookmark onto a conflicted copy.
  Resolve the conflict with a forward merge instead of a rewrite — merge the branch's own
  bookmark with the destination in one new commit, so nothing existing is rewritten and nothing
  built on your prior commits, in this tree or another, ever moves:

  ```bash
  jj -R "$LEGION_WORKSPACE" new legion/<KEY> main@origin -m "merge: resolve conflict against main@origin"
  ```

  Merge from the bookmark, never from `@`: a handoff split leaves `@` an empty, undescribed
  commit above the described one the bookmark already names, and `jj git push` refuses to push
  any commit without a description — merging from `@` drags that undescribed commit into the
  ancestry and the push fails (`Won't push commit … since it has no description`); the bookmark
  is always on a described, already-pushed commit. If the merge conflicts, resolve it in that
  one commit — edit the markers directly; there is nothing to squash, since the merge is the
  only new commit. Then `jj -R "$LEGION_WORKSPACE" new` to move off it, and push with the one
  push procedure (*Every role pushes its own commits* in `skill://legion-worker`): the merge descends from both the
  bookmark's old position and the destination, so it is a genuine fast-forward and *Rewriting
  pushed commits* never applies — nothing was rewritten, so there is no tip to record first.
- **After a retarget.** Retargeting a pull request to a new base does not re-run Tests. Merge the
  bookmark onto the new base (`jj new legion/<KEY> <new base> -m "<message>"`) and push with the
  ordinary push procedure — a genuine fast-forward, never the procedure for rewritten commits —
  so the new head runs Tests against the new merge result, and cite that run in the PR body.

## The unchanged-diff check

The fingerprint every role compares after a conflict-forced rebase (every flag and the fileset
verified on jj 0.45.1):

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && \
  jj -R "$LEGION_WORKSPACE" diff --from "fork_point(main@origin | <head-sha>)" --to <head-sha> \
    --git --context 0 '~(.legion | docs/solutions)' \
  | sed -e '/^@@/d' -e '/^index /d' | sha256sum
```

- `<head-sha>` is a full commit SHA; a jj commit id is the git SHA GitHub shows.
- `fork_point(main@origin | <head-sha>)` is the base the branch was cut from *at that head*:
  the old base for the pre-rebase head, the new base for the rebased one, so one command
  serves both sides. On a stacked PR substitute its base branch for `main`
  (`legion gh -- pr view <n> --json baseRefName`).
- A head the rebase hid is still addressable by its SHA in the shared workspace. A SHA the
  workspace cannot resolve (`jj -R "$LEGION_WORKSPACE" log -r <sha>` errors) counts as a
  changed diff — never as unchanged.
- `--context 0` drops context lines; the `sed` drops `@@` hunk headers (line positions move
  on a rebase) and `index` lines (blob ids move when the base's copy of a file changed). What
  is left is exactly the added and removed lines per file.
- The single fileset `'~(.legion | docs/solutions)'` leaves out the handoff ledger and retro's
  learnings: process artifacts the merge gate already exempts from re-review
  (`skill://legion-worker/references/merge-gate.md`), which change
  between one role's verified head and the next without changing the product. This is what lets
  each role compare against *its own* last verified head instead of trusting another role's
  numbers. It must be one expression: jj unions positional filesets, so two separate
  `'~.legion' '~docs/solutions'` arguments select every file and exclude nothing. Once
  `.legion/` is gone, jj warns `No matching entries for paths: .legion` on stderr; the hash is
  unaffected.

Where each role gets its two heads: the implementer — the tip before and after its own rebase;
the tester — the head its `E2E` line names and the new head; the reviewer — the `commit_id` of
its last submitted review (`legion gh -- api repos/{owner}/{repo}/pulls/{n}/reviews --jq '.[] | {commit_id, state, user: .user.login}'`)
and the new head; the merger never computes a fingerprint — it uses the `--summary` check in
`skill://legion-worker/references/merge-gate.md`.

## Rewriting pushed commits

**Rewriting pushed commits** — a `jj squash --into` a commit already on GitHub, or any other
rewrite of a commit you already pushed — is the LEGION-118 hazard in a second shape: jj rebases
every descendant of any commit it rewrites, and in the one shared repository a descendant can be
another tree's branch stacked on your pushed commit, which then moves, with its bookmark, onto a
rewritten copy. So look for a descendant outside your own chain first, and record the pushed tip
— which the rewrite leaves outside `::@-` — after a fetch and while your chain still descends
from it:

```bash
cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" git fetch && \
  foreign=$(jj -R "$LEGION_WORKSPACE" log --no-graph -T 'commit_id.short() ++ "\n"' \
    -r 'descendants(<the commit you are about to rewrite>) ~ ::@') && \
  { [ -z "$foreign" ] || { echo "not mine, and descends from the commit to rewrite: $foreign" >&2; false; }; } && \
  behind=$(jj -R "$LEGION_WORKSPACE" log --no-graph -T 'commit_id.short() ++ "\n"' \
    -r 'remote_bookmarks(exact:"legion/<KEY>", exact:"origin") ~ ::@-') && \
  { [ -z "$behind" ] || { echo "legion/<KEY>@origin is at $behind, which @- does not descend from" >&2; false; }; } && \
  jj -R "$LEGION_WORKSPACE" log --no-graph -T 'commit_id' \
    -r 'remote_bookmarks(exact:"legion/<KEY>", exact:"origin")' \
    >"${TMPDIR:-/tmp}/legion-<KEY>-$LEGION_ROLE-rewritten-tip"
```

`descendants(<commit>) ~ ::@` is everything built on the commit you are about to rewrite that is
not on your own chain. Non-empty means the rewrite would move work that is not yours: do not
rewrite it. Put the change in a new commit on top instead, and report the listed commits to the
architect. On a two-workspace rig of this shape a `jj squash --into` a pushed commit reported
`Rebased 13 descendant commits` and moved a second issue's twelve commits and its bookmark; the
check above listed those thirteen and refused before anything moved.

Then rewrite, resolve, and push with the one push procedure (*Every role pushes its own commits*
in `skill://legion-worker`). It lets the remote branch sit on the
tip you recorded, which the rewrite replaced, and on nothing else: when another role pushed after
you recorded it, the push is refused. The push deletes the file.

Once a base is frozen for others to stack on, never rewrite it: fixes land as new commits on top,
and the PR body's `Chain` line records what is frozen.

