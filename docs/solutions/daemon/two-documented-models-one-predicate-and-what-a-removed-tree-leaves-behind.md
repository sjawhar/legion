---
title: "A released child stays in its parent's tree: catch two coexisting models by tracing the event, share one ownership predicate, and ask what a removed tree leaves behind"
category: daemon
tags:
  - admission
  - sub-architect
  - reducers
  - liveAncestorTree
  - boot-repair
  - role-claims
  - review-finding
  - smoke-rig
  - routeActive
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/reducers.ts, packages/daemon/src/daemon/legion-state.ts, packages/daemon/src/daemon/processes.ts, skills/legion-architect/SKILL.md
related_issues:
  - "LEGION-57"
  - "LEGION-86"
  - "sjawhar/legion#1024"
symptoms:
  - "spawn_worker({issue: <child>, role: architect}) answers 500 'Issue X does not belong to Legion tree Y' right after release_wave"
  - "a released child appears in GET /legion/v1/state under trees and admission.queue"
  - "the parent's sub-architect spawn 409s at /worker/started with 'Worker respawn must resume the same agent session' and burns a boot-timeout cycle"
---

# A released child stays in its parent's tree

## Context

LEGION-57 (PR #1024) made the daemon match the model every other part of Legion already
implemented: a child issue released by its parent's architect (`release_wave` → `todo`) is owned
by a **sub-architect** running as a phase worker inside the parent's tree, never admitted as a
root tree of its own. Before the fix, `reduceIssueUpdated` emitted `admit` for *every* `todo`
transition, so a released child got its own `trees` entry and admission-queue place,
`rootForIssue(child)` resolved to the child, and the parent's `spawn_worker` was refused. On the
live deployment LEGION-19's six children and LEGION-28's one had all become independent roots
without their architects knowing that was not the design.

## 1. Two documented models coexisted for weeks — trace the event, not the docs

The approved lifecycle design said a child's `todo` "drives admission"; the daemon `AGENTS.md`
said a released child "is admitted as its own tree"; a retro document from the previous week
(`dispatch-status-writes-one-writer-per-transition-and-the-pending-write-fence.md`, § 1) recorded
that as the working model and told the next worker to trust it. Meanwhile the recursion-depth
limit (`maxRecursionDepth`), the release route's tree-membership check (`treeContains`), every
parent-directed `child-*` wake, and `LEGION_TREE != LEGION_ISSUE` in a worker's environment all
implemented sub-architects. The reducer followed the first model; everything else the second. Each
half was internally consistent and documented, which is why it survived.

What would have caught it earlier, and what to do the next time two documents disagree about
who does something:

- **Ask where the two paths meet and read that code.** The meeting point here was
  `deliverToWorker`'s `rootForIssue(issue) !== treeKey` refusal: under the "own tree" model it is
  unreachable for a released child (the parent never spawns one); under the sub-architect model it
  is the guard that makes the skill's flow work. A check that one model makes dead code is the
  contradiction made visible.
- **A retro document that records "how it works today" is a snapshot, not a spec.** § 1 of the
  sibling document above correctly described the daemon's behaviour on 2026-09-13 and drew the
  wrong conclusion from it (that the behaviour was intended). When you record observed behaviour,
  say whether you verified it is the *designed* behaviour — and if a later tree retires that
  model, cross-link from the new document (this one) rather than rewriting history in the old one.
- **Grep the reducer for the status, then follow the effect to the process manager.** The
  previous retro already said this; it applies with the opposite sign here — the question was not
  "who writes `in_progress`" but "who *should*", and the answer lived in `processes.ts`
  (`issueDepth`, `LEGION_TREE`) rather than in `reducers.ts`.

## 2. One ownership predicate, shared, with the ancestor statuses spelled out

`liveAncestorTree(state, issue)` (`legion-state.ts`) walks the parent chain — never `issue` itself
— to the nearest ancestor recorded in `state.trees` and returns that tree unless its status is
`lingering` or `closed`. Both `reduceIssueUpdated` (`admitOnTodo`: no `admit` when an owner exists)
and the boot repair `ProcessManager.adoptOwnerlessChildTrees` call it. One exported function is the
only thing that stops the two paths forking again the next time someone changes what "live" means.

Why `dead` and `launch-failed` **own** while `lingering` and `closed` **orphan**: the first two are
transient states of a root tree that comes back — `dead` is mid-resurrection, `launch-failed` is
re-admitted by the controller's next `todo` — so treating either as an orphan would admit the child
as a root and recreate the bug the moment the parent returned. `lingering`/`closed` trees have an
architect that is gone for good. Nearest tree wins (a grandchild under a lingering legacy child
tree is an orphan even under an active root) because `rootForIssue` and `routeActive`'s `treeFor`
resolve the same way, and the root's `spawn_worker` for that grandchild would be refused anyway.

The reducer's orphan path keeps a `log` effect naming the parent and the nearest tree's status, so
an unexpected root admission of a child is explained in the daemon log rather than silent.

## 3. What a removed tree leaves behind — "a queued tree is inert" was true and insufficient

The boot repair removes every child tree that holds no process (`queued`, `launch-failed`,
`active` without a locator) from `trees`, `admission.queue`, and `admission.active`. The first
version's doc comment reasoned that such a tree is inert: no locator, no resumable session, so the
parent's first sub-architect spawn launches fresh over whatever `roles` entry it left. The tester's
rig run passed every acceptance line — and the reviewer's blocking thread showed the reasoning
missed a shape the rig never built: a `launch-failed` or active-without-locator tree has had
`/process/started` write `roles[roleToken(child, "architect")]` **with a `sessionId`**, and only
`closeTree` ever deleted that entry. Left in place, `launchWorker` mints the sub-architect's boot
token with `claim.sessionId` as `expectedSessionId`, the fresh session 409s at `/worker/started`
(`Worker respawn must resume the same agent session`), the boot watchdog retires it after
`worker_boot_timeout_seconds`, and only the second launch succeeds. The same residue makes the
architect skill's "no architect claim → spawn" gate skip the spawn entirely.

The rule for any code path that deletes a `TreeState` without going through `closeTree`: **ask
what role claims point at this tree and what else cleans them up.** The repair now revokes the
claim through `revokeRoleClaim` (the same chokepoint `closeTree` uses) and deletes it. The rig
missed this because its child had never been a launched tree — a `queued` tree really is inert;
a smoke fixture built from a live rig's history covers only the shapes that rig happened to
produce. The unit test (`index.test.ts`'s boot case) now seeds the stale claim explicitly and
asserts it gone from memory and from the last persisted snapshot; the direct table test in
`processes.test.ts` (`adoptOwnerlessChildTrees` over every tree shape) asserts the revoke reached
`revokeSessionCapability` — the daemon-level test cannot observe that, because capabilities are
in-memory and empty at boot.

One exposure is logged, not repaired, and the doc comment says so: a `launch-failed` tree is
reached from `dead` after `MAX_LAUNCH_FAILURES` resurrections, and nothing stops the phase workers
an earlier *confirmed* generation of that root spawned. They keep their locators and
`LEGION_TREE=<child>`; after the removal their tree-scoped credential routes 404, and the new
sub-architect's `spawn_worker` for those roles resumes rather than replaces them. The repair names
them in a second log line (`removed root tree still has worker claims with recorded panes`); watch
for it after a deploy.

## 4. Known gap, re-filed: every "tree's architect" publish resolves to the root

`routeActive` publishes to the issue's active phase worker if one exists, else to
`roleToken(state.project, tree.root, "architect")` — the **root**. So a sub-architect never hears
its own tree's wakes (`phase-complete`, `launch-failed`, `worker-queued`, `worker-started` for its
child's workers go to the root architect), and a parent's `child-*` wakes go to the parent's
*active phase worker* when one is running (the tester saw both `child-adopted` wakes on the rig
land on the root's planner). This is LEGION-86, filed through the controller; it is not solved
here. A worker touching routing should read that issue's spec before assuming a wake reaches the
role that can act on it.

## 5. Process notes from this tree

- **The smoke rig's `envoy` mode is not shareable across trees.** A sibling tree's rig was live
  against the shared `LEGSMOKE` Dispatch project, and the daemon filters Dispatch events only by
  project-key prefix, so two `envoy` rigs would cross-admit each other's roots. The README's
  isolation path — a scratch Dispatch server built from the checkout, publishing into the rig's
  own NATS, `SMOKE_WEBHOOK_MODE=none SMOKE_DISPATCH_INGRESS=rig` — ran the same daemon, tmux
  server, listener, and pinned OMP with only the Dispatch server private to the run. Reach for it
  first when another rig is up; it is not a lesser proof.
- **A conflict-forced rebase before testing keeps the fingerprint procedure out of the picture.**
  GitHub reported `CONFLICTING` before the tester started; rebasing then (onto LEGION-20's
  design-gate rewrite, six files integrated with main rather than around it) meant no reviewed
  head existed to compare, so no unchanged-diff check, no confirmation approval, and no re-test
  round. Rebasing after approval would have cost all three.
- **A Minor thread about one dead constant was a whole-codebase cutover, and that was right.**
  The reviewer flagged the `CHILD_ADOPTION_ENVELOPE` constant feeding `routeActive`'s never-read
  `_envelope` parameter. Removing only `childAdopted`'s argument would have had it fabricate the
  same dead value one call deeper; the parameter went, and with it every forwarder
  (`settleCiVerdict`, `prComment`, `review`, `pullRequest`, `dispatchEnvelope`, both envelope
  constants) across `reducers.ts`, `events.ts`, `resync.ts`, `issues.ts`, `index.ts`, and two test
  files — `ast_edit` for the call sites, `tsc` to find the now-unused parameters. Nineteen call
  sites, one commit, no behaviour change.
- **Two pre-existing inaccuracies fixed in passing** because the tester read the surfaces this
  tree touched: checkpoint 4's single-issue fallback labelled a decomposed tree "single-issue" in
  the window between `release_wave` and `spawn_worker` (now gated on no child being released), and
  the daemon `AGENTS.md` said an architect "is never `phases[issue].phase`" — a sub-architect is,
  from its assignment until it spawns the child's first phase worker; the idle-retire exclusion is
  `claim.role === "architect"`, an explicit role check.

## Related

- `dispatch-status-writes-one-writer-per-transition-and-the-pending-write-fence.md` — § 1 records
  the retired model as it was observed; § 2 (the `statusAtRecord` fence) still applies, and the
  child's `in_progress` write at its first sub-architect spawn reads through it.
- `../testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md` — the rig, and its limit shown
  here: it proves the shapes it builds.
- `../../packages/daemon/src/daemon/AGENTS.md` — the "Children never take admission slots"
  invariant, the `/worker/spawn` row, and the boot ordering with `adoptOwnerlessChildTrees()`.
- `../../skills/legion-architect/SKILL.md` — "Release admits nothing", the `child-adopted` and
  `child-status` wake rows, and the `catchup-overseer` reconciliation clause.
