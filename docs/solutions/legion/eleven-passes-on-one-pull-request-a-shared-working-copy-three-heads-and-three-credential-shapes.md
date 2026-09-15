---
title: "Eleven passes on one pull request: working a shared jj workspace whose working copy is another role's, the three heads as they looked on this branch, and the credential-delivery arc from the implementer's seat"
category: legion
tags:
  - legion
  - jj
  - shared-workspace
  - rebase
  - three-heads
  - fingerprint
  - credentials
  - LEGION_GRANT_FILE
  - flakes
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-16"
  - "LEGION-12"
  - "LEGION-33"
  - "LEGION-37"
  - "LEGION-45"
  - "LEGION-52"
  - "LEGION-54"
  - "LEGION-60"
  - "sjawhar/legion#961"
---

# Eleven passes on one pull request

LEGION-16 (pull request #961) ran eleven implementer passes over three days: eleven rebases onto
`main` (five of them conflict-forced in the last three passes, two inside one hour), six review
rounds with 35 threads, ten tester rounds, and three delivery mechanisms for the same credential. The code lessons are in
`../daemon/interactive-controller-upgrade-the-headless-pane-probes-alive-and-is-killed-once-after-the-restart.md`
and `daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md`.
This note keeps the operating practices that made the passes survivable, each stated once with
where it came from. The hazards of the shared repository as such — one operation log, repo-scoped
identity, the active-phase overwrite — are in
`shared-main-repo-hazards-for-concurrent-issue-workspaces.md`; nothing here repeats them.

## 1. A shared workspace whose working copy is not yours

From the eighth pass on, the shared issue workspace's working copy (`@`) was the tester's: an
empty commit carrying only `.omp/config.yml` on top of the tester's record. Every rule below
follows from one decision — never move it.

- **Rebase by explicit commit id**, `jj rebase -b <commit id> -d main@origin`, not
  `roots(main@origin..@)`: `@` was not on the chain being moved, and after LEGION-45 the architect
  ruled that a rebase names what it moves. The tester's and reviewer's record commits move with
  the chain because they are descendants of it.
- **Resolve conflicts at the commit that raised them, in place**:
  `jj resolve -r <commit id> --tool <one-shot copy tool> <path>` with a merge tool configured
  as `cp <prepared file> $output`. The prepared file is built from the conflict markers — take
  the rebase-destination snapshot and apply the branch's own diff side to it — so both intents
  survive by construction. Working through `jj new <conflicted>` + `jj squash` would have moved
  `@`. One trap: a tool that writes the file verbatim drops a missing final newline; the
  fingerprint diff shows it as `\ No newline at end of file` — check for that line before pushing.
- **Verify in a scratch jj workspace**, `jj workspace add --name <n> -r <commit> /tmp/<n>`, run
  the suites there, and fold any fix back with `jj -R /tmp/<n> squash --into <commit id> -u
  <paths>`. Forget and remove it when done (`jj workspace forget <n>`). Every squash rewrites the
  descendants, so the shared workspace and any other scratch workspace go stale: `jj workspace
  update-stale` after each one, and re-read commit ids — never reuse an id from before a squash.
- **Divergent change ids are normal** on a branch other workspaces still point at (old copies stay
  visible); address commits by commit id and leave the divergence alone. `jj abandon` is off the
  table on the shared repository anyway.
- **Push by bookmark**, `jj bookmark set legion/<KEY> -r <commit id> --allow-backwards` then
  `jj git push --bookmark legion/<KEY>`; never commit `.omp/config.yml`.

The same discipline made two conflict-forced rebases inside one hour cheap: all conflicts were in
the daemon `AGENTS.md` and one doc comment, each resolved in place in minutes, with the suites
re-run once in the scratch workspace.

## 2. The three heads, as they looked on this branch

The controller skill's merge gates compare three commits: the **verified** head the tester ran
the smoke rig against, the **approved** head the reviewer named, and the **current** head the
merger publishes. On this pull request they were 7b2d3f1c (tester round ten), e8d14dd3 (round-six
approval), and — after retro — the commit above it. Between the first two sat: the `.legion/`
deletion (four files, nothing else), two conflict-forced rebases, and one documentation commit
answering round five. The reviewer accepted that gap because the unchanged-diff fingerprint
(`unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md`) reduced it to exactly one
non-comment code line — `main`'s `spyOn` joining this branch's `vi` on a `bun:test` import — and
the two rebase comments named that line; the architect ruled no tester delta was owed for it.

What generalises: **a rebase changes the fingerprint; what the reviewer judges is the diff of the
two diffs.** Compute both sides against their own fork points, `diff` the two outputs, and post
the non-markdown lines in the rebase comment. If they are empty or a handful of lines you can
explain, the approval is a confirmation; if not, it is a round. And **retro commits after
approval**, so approved ≠ current by design — the merger's `jj diff --from <approved> --to <tip>
--summary` must list only `docs/solutions/` paths, which is why nothing else may move in retro.

## 3. One active phase per issue, from the worker's seat

`legion handoff complete` answered 409 (`Phase for LEGION-16 is no longer owned by this worker`)
on three consecutive passes, because the tester had been assigned meanwhile and an issue has one
active phase (LEGION-37; the daemon-side rule is in
`../daemon/one-writer-for-the-active-phase-and-bystander-catchups.md`). The completion is not
lost: report it to the architect with `envoy_publish` on its role topic and stop; the deployment
instructions now say exactly that. The ninth pass's `handoff complete` succeeded first time, the implementer being the active phase
again — so the 409 is a sequencing signal, not an error to retry.

## 4. A queued spawn prompt that never arrives

On this tree the architect's `spawn_worker` for this implementer came back `resumed` (the pane
was live and idle) and the task never reached the session; a direct Envoy message to the
implementer's role topic did. LEGION-60 found and fixed the daemon side (a prompt is delivered when the turn
starts — `../daemon/a-prompt-is-delivered-when-the-turn-starts-not-when-the-shim-acknowledges.md`).
For an architect on a daemon that predates it: a `resumed` spawn with no visible turn within a
minute is not a slow worker; send the task as a message and let the daemon catch up later.

## 5. Three credential shapes in one day, and the surface that showed each

The controller's `legion gh` needs a short-lived grant. In one day it travelled three ways:

1. **In the command text** (a prelude the extension prepended). The model imitated the prelude
   into later commands (LEGION-12), and the imitation failure rate was the tester's headline.
2. **In the bash tool's `env` argument.** Invisible to the model — and dropped entirely by the
   secretsd plugin's legacy bash shim, which replaces the bash tool and discards `env`
   (LEGION-52). In a production-like pane the grant never reached the shell.
3. **In a file the pane names** (`LEGION_GRANT_FILE`, written 0600 before each command;
   LEGION-54). Nothing rides the tool call; the tester's round eight counted 22 bare
   grant-consuming calls across controller and worker panes with zero hand-written credential
   lines and zero failed redemptions.

Shape 2 passed every unit test and an extension-only rig. It failed only on the smoke rig that
loads the real profile plugin tree — `~/.omp/profiles/legion/plugins`, secretsd included
(`../testing/proof-rig-loads-the-production-plugin-tree.md`). The lesson is not "the file shape is
right" (it is) but **which surface counts**: for anything that rides a tool call through OMP, the
proof is a pane on the production plugin tree, because another plugin may own the tool. The
deployment instructions made that rig the implementer's own pre-merge proof for daemon changes.

## 6. Flakes in packages the branch does not touch

Two CI failures on this pull request were in packages with zero diff against `main` — a Playwright
timing test in the Dispatch web client (`doc.e2e.ts`, two users see each other's edits within one
second) and a Go test in the Dispatch server (`TestMarkAnchorVerifiesBrowserMark`) — both of which
had failed on `main` itself the same day. The rule the architect set: **re-run the failed job at
the same head, once; do not push over a flake; record both run ids and the `main` evidence in the
pull request body's CI line**, and if the re-run is red on the same test, report and stop. The
daemon counts a red verdict on a code head as a fix attempt (LEGION-33) and published `pr-blocked`
for it; the record in the body is what lets the reviewer and the merger read that count correctly.

## 7. One coordination pattern worth copying

The Envoy role-claim bridge (`packages/pi-envoy/extensions/envoy.ts`) used to be a single
process-wide slot; a `task` subagent inside the controller pane loads its own instance of the
extension into the same process and took the slot, so the pane's own re-claim after `/new` went
to the subagent. The fix keys the bridge on the **live** session id read from
`context.sessionManager.getSessionId()` at call time, one entry per instance. In pi-envoy, any
process-wide registry must assume several extension instances per process and address them by a
live identity, never a captured one.

## Related

- `shared-main-repo-hazards-for-concurrent-issue-workspaces.md`: the hazards this note's
  practices defend against.
- `conflict-only-rebases-keep-the-diff-auditable.md` and
  `completed-phase-touches-nothing-and-conflicting-is-reported-not-rebased.md`: when a rebase is
  allowed at all.
- `two-envoy-rigs-on-the-shared-legsmoke-project-cross-admit-and-a-branch-behind-mains-plugin-contract-gets-its-own-profile.md`
  and `teardown-keys-on-the-rig-directory-and-tests-ownership-exactly.md`: the rig lessons this
  tree's tester hit (two envoy-mode rigs on one Dispatch project; teardown keyed on the rig's own
  identity), recorded there.
- `controller-gate-2-required-checks-live-reads.md`: the merge queue's gate 2 as verified live.
