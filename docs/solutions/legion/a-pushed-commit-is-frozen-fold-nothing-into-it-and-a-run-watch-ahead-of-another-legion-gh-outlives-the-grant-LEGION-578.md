---
title: "A pushed commit is frozen (fold nothing into @- after legion push; recover a squash into it forward), and a run watch ahead of another legion gh call outlives the grant"
category: legion
tags:
  - jj
  - divergent-change
  - legion-push
  - legion-gh
  - grant-lifetime
  - worker-pod
date: 2026-10-08
status: superseded by LEGION-631 — `legion push`, `legion gh` and the per-command grant are gone; every role pushes with plain `jj bookmark set legion/<KEY> -r @-` and `jj git push --bookmark legion/<KEY>`, runs plain `gh` as its App from `GH_CONFIG_DIR` with no grant to outlive, and the plugin no longer refuses `jj undo` or `jj abandon`, while the frozen-commit rule itself (a new commit above `@-`, never a squash into it) still holds
module: legion
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
---

# A pushed commit is frozen (fold nothing into @- after legion push; recover a squash into it forward), and a run watch ahead of another legion gh call outlives the grant

Extends docs/solutions/legion/worker-pane-shell-gotchas.md (§1 on the grant's 60-second life, §15 on
a `(divergent)` twin after `jj squash --into`).

## After `legion push`, `@-` is the remote's commit: a new commit above it, never a squash into it

- `legion push` leaves `@-` on the commit the remote bookmark names. `jj squash --into @- <paths>`
  then rewrites a pushed commit: the change id gains a second visible commit (`(divergent)` on both),
  the local chain no longer contains the pushed commit id, and the next `legion push` is a rewrite of
  pushed history — the procedure `skill://legion-worker/references/conflicts-and-rewrites.md` keeps
  for conflict rebases, not for a handoff amendment. Fold a late edit into a **new** commit
  (`jj split -m "…" <paths>`); one more commit above is free.
- The pi-legion extension refuses `jj undo`, `jj abandon` and every `jj op` rewrite in a worker's
  bash command (the operation log is shared by every issue workspace of the clone), so the recovery
  is forward and by commit id, since the divergent change id no longer selects one revision:
  1. `jj new <pushed commit id>` — the remote bookmark still names it, so it is intact;
  2. recreate the content there and `jj split` it into its own commit;
  3. `jj squash --from <the rewritten twin> --into <that new commit> --use-destination-message`.
     The twin is your own pushed tree plus the folded edit, so the squash conflicts only on what the
     two copies of the edit differ in (here a `completed` timestamp); resolve by
     `jj restore --from <the new commit> --to @ <path>`, then `jj squash` to fold the resolution
     into the conflicted commit. Afterwards `jj log -r 'divergent() | conflicts()'` shows none of
     yours and `legion push` is a plain fast-forward.
  This differs from gotchas §15, whose twin held *other* content (a pre-squash working copy) and
  stays untouched; squash the twin only when it is the rewritten copy of your own pushed commit.

## One `legion gh -- run watch` ahead of a second `legion gh` in the same bash call

- The extension mints one grant per bash call; it lives 60 seconds (`credential.ttl`), or five
  minutes only when the call runs `legion push` (`pushTTL`; `packages/daemon/internal/api/credentials.go`).
  A `run watch` that waits for CI takes minutes, so a `legion gh -- run view` or `pr view` after it
  in the same call redeems an expired grant: `Unable to redeem LEGION_GRANT: daemon returned 403:
  {"code":"GRANT_EXPIRED"}` — with the watch's own exit code already printed. Put the watch last in
  its call, or alone; the next bash call gets a fresh grant. Gotchas §1 names this shape for a slow
  command ahead of a push; a long GitHub read ahead of another GitHub read is the same shape with no
  five-minute grant to save it.

## Evidence

sjawhar/legion#1846, conflict round. After `legion push` had moved the bookmark to 7721ffa0 (the
notice-comparison test fix), the implementer ran
`jj squash --into @- -m "implement: record handoff (conflict round)" .legion/LEGION-578/implement.json`;
`jj log` then showed `a89a38f1 rtkqxspx DIVERGENT | implement: record handoff` as `@-` and
`7721ffa0 rtkqxspx DIVERGENT | test(daemon): …` under `legion/LEGION-578@origin`. `jj undo` was
refused by the extension. Recovery: `jj new 7721ffa0…`, the handoff rewritten and `jj split` into
3399c9aa, `jj squash --from a89a38f1 --into 3399c9aa --use-destination-message` (one 2-sided
conflict, the `completed` line), `jj restore --from 3399c9aa --to @ .legion/LEGION-578/implement.json`,
`jj squash` → 8842783b with 7721ffa0 as its parent; `jj log -r 'divergent() | conflicts()'` listed
only another tree's commits; `legion push` moved the bookmark forward from 7721ffa0 to 8842783b.
The architect was told in the round's report. Round 4: one bash call ran
`legion gh -- run watch 37855982280 … --exit-status` (188 s wall time) and then
`legion gh -- run view 37855982280 --json jobs`; the watch printed `tests@f98f84e7 exit=0`, the
view got `GRANT_EXPIRED`, and the same view in the next call succeeded.
