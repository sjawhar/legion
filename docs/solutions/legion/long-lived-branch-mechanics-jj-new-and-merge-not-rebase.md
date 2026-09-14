---
title: "Long-lived issue branch mechanics: run jj new after every push (the .omp/config.yml divergence), merge main rather than rebase when it moves, expect the 409 on every resumed round after another role ran, and keep the two test suites apart on a loaded box"
category: legion
tags:
  - legion
  - jj
  - divergent-change
  - omp-config
  - merge-commit
  - conflict-resolution
  - handoff-complete
  - LEGION-37
  - flaky-tests
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-33"
  - "sjawhar/legion#993"
  - "LEGION-37"
  - "LEGION-45"
symptoms:
  - "jj log shows the implementer's handoff commit twice: `msvksvmx/0f5b4e39 (divergent)` and the pushed copy"
  - "jj: `Change ID kulvkspq is divergent` — the two copies differ only by an empty `.omp/config.yml`"
  - "PR reads `mergeable: CONFLICTING` / `mergeStateStatus: DIRTY` after main moved, conflict is the same AGENTS.md bullet list every time"
  - "legion handoff complete: Unable to report phase completion (409): Phase for <KEY> is no longer owned by this worker"
  - "one process-manager test fails on the first full `bun test`, never again; `go test ./internal/integration` fails only while `bun test` runs beside it"
---

# Long-Lived Issue Branch Mechanics

LEGION-33's implementer session was resumed four times over one branch (open the PR, merge main
after a conflict, land review rows plus a second merge, push the `.legion/` deletion) and then
revived for this retro. None of the following is about the change; all of it is friction a future
worker on a long-lived branch can skip.

## 1. `jj new` immediately after every push — or jj's snapshot makes the pushed change divergent

Until LEGION-58 the daemon's workspace provisioning — not OMP — wrote an empty `.omp/config.yml`
into every issue workspace, and `.omp/` is not in `.gitignore`. jj snapshots the working copy on
every command, so if `@` is still the commit you just pushed, the next jj command amends that
commit locally with the untracked file — producing a second copy of the same change id (`jj log`
marks both `(divergent)`), while the remote holds the first copy. On this tree it happened twice
(`kulvkspq`: pushed `2c808fbd` vs local `e5cf55e3`; `msvksvmx`: pushed `f7761e36` vs local
`0f5b4e39`), each time because the handoff commit was pushed and the session then went idle with
`@` still on it.

Discipline: `jj bookmark set … && jj git push && jj new` as one sequence, so `@` leaves the pushed
commit before anything can snapshot into it. Recovery when it has already happened: confirm which
copy is on the remote (`jj log -r 'change_id(<id>)'` with
`self.contained_in("ancestors(legion/<KEY>@origin)")`), confirm the stray differs only by
`.omp/config.yml` (`jj diff --from <stray> --to <pushed> --stat`) and has no descendants, then
`jj abandon <stray commit id>`; never `jj undo`/`jj op restore` in an issue workspace
(LEGION-45). The `jj split -m … <paths>` habit in
[worker-pane-shell-gotchas](worker-pane-shell-gotchas.md) keeps the file out of your own commits
but does not prevent this — the snapshot lands after the push. Fixed in LEGION-58 by removing the
write: OMP's project directory is simply the pane's cwd (no marker file), an absent project config
is read exactly like an empty one, and OMP does not create the file on boot, so in a workspace
provisioned after that daemon deployed nothing is left for an idle session to snapshot (a tree
provisioned before it keeps its copy until it closes). `jj new` right after a push is still the
habit for any other untracked content.

## 2. `main` moved twice; a merge commit each time, and the conflict was the same bullet list

Sami's rule against rebasing to refresh CI does not apply to a genuine conflict, but a merge
commit (`jj new <branch head> main@origin`, resolve, describe) is still the better resolution:
the reviewed commits keep their SHAs, the reviewer's approval and the tester's evidence stay
pinned to real commits, and the PR's diff against main is unchanged. Both times the only conflict
was `packages/daemon/src/daemon/AGENTS.md`'s `## Operational invariants` list — this PR appended a
bullet after "Boot never resurrects trees", and main appended (then reworded) bullets at the same
spot. Resolution both times: main's bullets in main's wording first, this PR's bullet last,
nothing altered on either side. Then verify two diffs before pushing: `jj diff --from main@origin
--to @ --stat` must be exactly the PR's file list, and `jj diff --from <previous branch head> --to
@ -- <conflicted file>` must show only main's edits. Record the conflicting files and the rule
you applied in the PR body's "Merge with main" section; the merger reads it.

If main had bumped `LegionState.version` or touched `reducers.ts`, the migration would have had to
be renumbered and re-chained after main's — check `jj diff --from 'fork_point(main@origin |
legion/<KEY>@origin)' --to main@origin --stat -- <your files>` before resolving, and say the
answer ("no state bump on main, migration stands") in the PR body either way.

## 3. `legion handoff complete` 409s on every resumed round after another role has run

Rounds 1 and 2 (open the PR; merge main) reported completion normally. Rounds 3 and 4 (review
rows; `.legion/` deletion) — both after the tester and reviewer had been spawned on the issue —
answered `409 Phase for LEGION-33 is no longer owned by this worker` although the same session had
just pushed the branch and edited the PR. This is LEGION-37, documented in
[worker-pane-shell-gotchas § 11](worker-pane-shell-gotchas.md) and
[shared-main-repo-hazards § Hazard 3](shared-main-repo-hazards-for-concurrent-issue-workspaces.md):
`phases[<KEY>]` no longer names the implementer. Do exactly what the deployment instructions say:
do not retry, do not write a second handoff, `envoy_publish` the summary the command would have
carried to `notifications.role.legion-<project>-<KEY>-architect`, naming the 409 so the architect
knows no `phase-complete` will arrive. On this tree the architect accepted each message as the
completion report (and, per its retro brief, moved the Dispatch status by hand). Plan for it:
after the first hand-off to another role, every later `handoff complete` from the implementer
will 409 until LEGION-37 lands.

## 4. Two heavy suites on one loaded box: run them apart, and re-run a lone flake before believing it

The first full `bun test` in `packages/daemon` showed one failure in the process-manager suite
(`ECONNREFUSED` on a controller shim socket, `tmux split-window did not report a pane id`) that
did not reproduce across four later runs; every CI run of the same code was green. In the same
minutes `go test ./...` in `packages/envoy`, run concurrently, failed only in
`internal/integration` (embedded-NATS reconnect timeouts) and passed alone (`ok … 170.556s`).
The box was carrying other Legion trees' work at the time (the architect's brief puts the load
around 140 on 32 cores). The watchdog/deadline tests observe real sockets and tmux panes, so
they are the first to lose under load. Rule: run the daemon and Envoy suites sequentially, and
when a single process-manager test fails once, re-run the suite before reading the failure as
yours — then record both facts (the flake and the clean re-runs) in the handoff rather than
only the clean count.

## 5. Bash-tool grants on a pane still running the pre-LEGION-12 plugin

This pane ran with `omp-18.1.15-sami.9bff2014-rpcfix` and the old plugin behaviour from
[worker-pane-shell-gotchas § 1](worker-pane-shell-gotchas.md): every bash call had one to three
`export LEGION_GRANT='…'` lines injected and only the **first** redeemed; the later ones answered
`Unable to redeem LEGION_GRANT (403)`. A `DEBUG` trap that captured the first exported value and
re-exported it before `legion gh` worked, with two edges worth knowing: the trap is cleared when a
call is interrupted (a timed-out `gh run watch` cleared it; reinstall before the next `legion gh`),
and a grant does not outlive a long call — a 500-second CI poll loop 403'd from its second
iteration on. Poll CI in short single calls or `legion gh -- run watch <id> --exit-status` inside
a bounded timeout, never a foreground sleep loop. On a pane launched with the fixed plugin none of
this applies; check `LEGION_OMP_PATH` and the plugin version before reaching for the trap.
