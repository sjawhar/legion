---
title: "A state that starts lasting days gets a census of every inbound fact, and the rule re-entered on it tells nobody"
category: daemon
tags:
  - workflow-engine
  - awaiting-merge
  - merged-root
  - notices
  - inbound-facts
  - re-entry
date: 2026-10-25
status: active
module: packages/daemon/internal/workflow
related_issues:
  - "INFRA-229"
  - "sjawhar/legion#2180"
---

# A state that starts lasting days gets a census of every inbound fact, and the rule re-entered on it tells nobody

Extends docs/solutions/daemon/a-rule-on-a-state-that-stands-for-the-head-is-applied-by-every-fact-that-can-make-it-stand-census-the-predicates-writers-LEGION-698.md.

That note censuses the writers of a rule's predicate. The delta: a change that turns a short state
into a long one (here a merged root standing in `awaiting_merge` for days while its tree runs) has
a second census to take, of every fact that can land on the issue while it stands there.

- List every intake handler that can fire on the issue in that state before the first round
  completes: `opened`, `reopened`, `synchronize` and `push`, `closed`, a red check, a merge-queue
  failure, a Dispatch status move, and the boot sweep. For each, say what it does to the state and
  give the ones that must leave it standing a test. A window that used to be milliseconds wide
  never needed this; one days wide meets every one of them.
- The exits are not the census. Auditing every way out of the wait (every exit runs `leave`) says
  nothing about the facts that arrive while it stands.
- A rule that is re-entered (on every member's departure, on every boot) writes nothing that never
  de-duplicates. A `notice` writes one row per call (`effects.go`), so the notice goes only where
  the state is newly entered (`closeMergedTelling`, called from `merged` and from `transition`'s
  already-merged branch) and never inside the rule (`closeMerged`), and a test holds that a
  re-entry adds no row.

## Evidence

- The plan audited the wait's exits and placed the notice correctly, but no one listed the inbound
  GitHub facts. The tester found that an `opened` for a new pull request from the waiting root's
  branch made `pullRequestOpened` write the open pull request over the recorded merged one; when
  the last child left, `closeAwaitingRoot` read `state != merged` and the root never closed,
  holding its slot and workers. Before the change the root closed at its merge, so the window did
  not exist.
- The fix is a guard at `packages/daemon/internal/workflow/pullrequest.go:52-56`: while the issue
  awaits its merge (`awaitsMerge`) and the recorded pull request is merged, an opened or reopened
  of another number is logged and not recorded. It is scoped to that state because an issue set
  back to `todo` reruns, and its new pull request must still replace the merged one.
- `synchronize` was already neutral there (`withdrawMovedReady` returns on a merged pull request,
  `ready.go`); its comment lists the neighbouring doors and was the census the plan could have
  copied.
- Live proof: `scripts/e2e/merged-root-waits.sh` step 7 drives the `opened` envelope and holds the
  root on its merged pull request for 3 s. With the guard removed it fails with the tester's
  symptom. Step 5 restarts the daemon at the waiting state and holds the notice count at 1.
