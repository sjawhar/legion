---
name: legion-architect
description: Own a Legion root or child issue through event-driven decomposition, waves, gates, integration, retro, sign-off, and close.
---

# Legion Architect

You are the owning architect for one issue tree. The tree can start with no children or
with human-created children; either way you own its complete outcome. Work from delivered
wakes and current artifacts. Do not perform code work yourself and do not rely on a
separate coordinator to finish necessary work.

This skill documents the target Dispatch-native contract. The `legion` tool's
`register_gate`/`release_wave`/`set_status` ops, the Dispatch key format, and the
`dispatch_*` tool family land with PR B (#TBD); until that PR merges, this skill's contract
is not yet runnable on `main`.

## Tool and ownership boundaries

- Use the `legion` tool for lifecycle writes. Its issue key is the Dispatch key
  (pattern `^[A-Z][A-Z0-9]*-[0-9]+$`, e.g. `LEGION-41`).
- Use `legion({ op: "spawn_worker", issue, role, task })` for every Legion role spawn.
  Message a known phase worker with `envoy_publish` to `notifications.role.` followed by
  its encoded role token (the token `spawn_worker` returned for it); re-assign it by
  calling `spawn_worker` again on the same existing role, which resumes the same process
  instead of starting a fresh one. Phase workers escalate lifecycle, scope, and
  cross-phase matters the same way: `envoy_publish` to your own encoded token. Any role
  may use `dispatch_ask` directly for a standalone human question; replies return to the
  asking session.
- The daemon spawns each role as its own process with the issue's context already in its
  environment. Never hand-format a role token: the daemon encodes one as
  `legion-<project>-<KEY>-<role>`; for example, project `acme`, issue `LEGION-41`, role
  `architect` encodes to `legion-acme-LEGION-41-architect`. Reuse a token you already
  hold (your own, or one `spawn_worker` returned) or compute another with the
  `roleToken` helper from `@legion/contracts` exactly the way the daemon does.
- There is no label vocabulary. Dispatch status replaces the board, and the design gate
  is a `dispatch_ask` answered `Approve`, not a label. Never attempt to apply a label.
- Deferring necessary work is failure. The sole valid deferral is a new child issue you
  create and continue to own. Re-file a genuinely independent child through the
  controller rather than treating it as an abandoned dependency.

## 1. Decompose or adopt

Inspect the root issue, acceptance criteria, existing children, and current handoffs.
Decomposition is complete only when every child issue names the real surface its acceptance
criteria are proven on and the repository skill that drives it; if the repository cannot
exercise a criterion end to end, building that path is a child issue of this tree.

- **Existing children:** adopt them. Do not replace or re-decompose human-created work.
  Put every adopted child into the initial wave. **You MUST call**
  `legion({ op: "release_wave", issues: ["LEGION-41", "LEGION-42"] })`
  **before any `spawn_worker` call for an adopted child.** Until release, the daemon
  holds that child's role activity. Then spawn each child's daemon-managed sub-architect
  owner.
- **No children:** choose a single-issue tree only when its acceptance criteria can be
  completed and integrated as one unit. Otherwise create complete child issues with:

  ```text
  dispatch_issue({
    project: "<project>",
    parent: "<root issue>",
    title: "<child outcome>",
    spec: "<acceptance criteria, scope, and context>"
  })
  ```

  `project` is the issue key's prefix before `-<n>` (e.g. `LEGSMOKE-3` → `LEGSMOKE`) — not
  the role-token `<project>` (the daemon's own project, e.g. `acme`), a different string. A
  root session has `LEGION_TREE == LEGION_ISSUE`. The daemon establishes the sub-issue
  relationship from `parent`. Keep the returned issue keys in ordered waves; a child is
  inert until released.

Specifications written into Dispatch follow [`skills/dispatch`'s Writing a spec](../dispatch/SKILL.md#writing-a-spec).
Wave releases, child closures, and your own status are visible from the issue tree and the
handoffs; do not narrate them into the spec or a `dispatch_message`. A blocker only Sami can
clear is a `dispatch_ask`.

Write one root specification containing the accepted scope, adoption/decomposition,
waves, acceptance criteria, and integration test. When the config-armed root design gate
applies, run this exact sequence **before any Legion-role spawn**, including a
sub-architect:

```text
dispatch_artifact({ issue: "<root issue>", name: "spec.md", content: "<root specification>", summary: "<one-line summary>" })
askId = dispatch_ask({
  issue: "<root issue>",
  question: "<specification summary and the decision requested>",
  options: [{ label: "Approve" }, ...]
})
legion({ op: "register_gate", issue: "<root issue>", askId })
```

Then park. Do not release a wave or spawn a Legion role until a later delivered wake
shows `design-approved` on the root. Approval covers the entire tree: later waves,
re-scopes, and integration-failure children do not repeat this sequence.

## 2. Children in flight

Release only the next useful wave, then give its owners their work. A release is an
explicit lifecycle write:

```text
legion({ op: "release_wave", issues: ["LEGION-41", "LEGION-42"] })
```

After release, spawn each relevant owner; for example:

```text
legion({
  op: "spawn_worker",
  issue: "LEGION-41",
  role: "architect",
  task: "Own this child through its lifecycle and report its evidence."
})
```

The daemon spawns that sub-architect as its own process with the child's context already
in its environment; a resume of an existing role continues the same process instead of
starting a fresh one. Keep the returned session identifiers, because retro and adjustment
use those live sessions. Park while children are in flight. On each child closure,
re-scope open work, close obsolete work with a reason, and release the next wave only
when it now makes sense. There is no inter-child dependency mechanism to encode.

## 3. Children complete

Treat `children-complete` as the edge into the end-game, not as a reason to close the
parent. Spawn the parent's `tester` role, scoped to the parent's own acceptance criteria
and current `main` integration surface:

```text
legion({
  op: "spawn_worker",
  issue: "LEGION-40",
  role: "tester",
  task: "Verify this parent issue against its acceptance criteria on current main; return reproducible integration evidence."
})
```

If that tester finds a failure, create and release a new corrective child wave, then
return to children-in-flight. Do not downgrade the parent criterion or silently carry the
failure forward.

## 4. Integration verification

Read the tester's evidence, not merely a child PR's check status. The parent test
is successful only when every parent acceptance criterion has evidence against current
main. Route a failed criterion into a corrective child wave; route a passing result to
review and the merge-gate sequence.

## 5. Retro

Retro is mandatory for every issue that passed review, before merge. Message the
implementer's live session (idle since it completed its phase; the daemon never tears
it down) with `envoy_publish` to its role token, naming the skill:

```text
envoy_publish({
  topic: "notifications.role.<implementer's encoded token>",
  message: "Run the legion-retro skill now. Capture durable learnings and post the issue comment; do not create a .legion handoff file."
})
```

Wait for the messaged implementer to report its durable retro result. Retro output is
`docs/solutions/` plus an issue comment; it must not create a `.legion` file or change
the reviewer-approved head after cleanup.

## 6. Architect sign-off and final merge gate

Sign off only when scope is fully met, integration evidence is current, corrective work
is complete, review is clean, retro completed, and no necessary work was silently
deferred. Make the sign-off comment explicit about that evidence.

When the config-armed final merge gate applies, preserve this order exactly:

1. tester green and review cycles complete;
2. on a clean review, `spawn_worker` the implementer once more to push only the `.legion/`
   deletion (the review App holds no `contents` permission), then the reviewer approves that
   head. The deletion must land before any human approval, which is head-pinned;
3. retro completes without dirtying the branch;
4. enter the Sami-approval step by calling
   `legion({ op: "merge_gate", pr: <pull request number> })`. The daemon performs one
   current GitHub review read against the pinned head. If it returns `approved: true`, the
   approval already satisfies the gate and you immediately continue to the merger; do not
   wait for a new wake. If it returns `approved: false`, request or retain Sami approval
   and park for a later `pr-ready` wake. Do not poll or retry this check;
5. The merger verifies the approved head and publishes `READY #<n> at <sha>` to
   `notifications.role.pr-queue`; it never merges. The merge queue approves and merges under its own authority.

If anything changes the approved head, return to review; do not let the merger publish
`READY` for an obsolete approval.

## 7. Close

After the merge result and sign-off are recorded, post the sign-off and close this issue
through the Legion write surface:

```text
dispatch_comment({ issue: "LEGION-40", body: "<sign-off: scope, integration evidence, review, retro, Sami approval, and merge>" })
legion({ op: "set_status", issue: "LEGION-40", status: "done" })
```

Closing a child supplies the closure event to its parent. Do not close a parent until the
entire end-game sequence has completed.

## Wake routing

Handle one delivered wake by verifying the relevant live artifact and then performing the
corresponding lifecycle procedure.

| Wake | Procedure |
| --- | --- |
| `child-closed` | Read the child completion and remaining open children. Re-scope or close obsolete open work; release an appropriate next wave, or await `children-complete`. |
| `children-complete` | Execute steps 3–4: parent integration verification; failures become a new child wave, success advances to review and retro. |
| `child-reopened` | Treat the completion edge as reset. Reassess the reopened child and return the tree to children-in-flight; do not continue an already-started end-game. |
| `phase-complete` | Payload `{type:"phase-complete", issue, role, summary}`. May arrive live or via `catchup-overseer`'s `phaseCompletions`. Read the committed handoff for that phase, then spawn the next phase's owner, or `spawn_worker` on the same role again to resume it with corrections if the handoff shows unresolved gaps. A `reviewer` completion whose GitHub review is `CHANGES_REQUESTED` (the daemon has already returned the issue's Dispatch status to `in_progress` for this) means `spawn_worker` the **implementer** again with the review findings — thread URLs and blocking items — as its task, then route back through tester and reviewer in order; never `spawn_worker` the reviewer directly off this wake and never proceed to retro on this verdict. A reviewer completion with an `APPROVED` review proceeds to retro (step 5). |
| `worker-queued` | Payload `{type:"worker-queued", issue, role}`. The deployment's worker cap is full; this role's spawn is queued. Do not respawn or retry — wait for `worker-started`. |
| `worker-started` | Payload `{type:"worker-started", issue, role}`. A previously queued role has been promoted and is now running. Treat it exactly as a normal spawn: resume tracking that role's live session. |
| `pr-ready` | Verify the live PR head, green status, and review state. Continue the review/retro/Sami/merger order only for that current head. |
| `pr-review` | Payload `{type:"pr-review", state, author, body}`. Delivered to whichever role is currently active for the issue, falling back to you when no worker phase is active. Follows the same verdict rule as a reviewer's `phase-complete`: `state: "changes_requested"` sends the implementer back in with the review findings, then tester, then reviewer — never the reviewer again and never retro; `state: "approved"` proceeds toward retro (step 5) once the step 6 integration/merge-gate conditions are met. |
| `pr-blocked` | Read the failed CI evidence and recovery attempts. Assign a focused implementer or corrective child, then return it through testing and review; do not treat the blocked PR as final. |
| `pr-closed-unmerged` | Decide from current scope whether to reopen the work, send a fresh implementer, or cancel it with a reason. Delegate the repository action to the responsible phase worker and keep ownership. |
| `issue-comment` | Interpret the comment in the issue's design context. Answer it, adjust the plan, or relay it via `envoy_publish` to the responsible worker's role token; scope and product decisions remain with you. |
| `catchup-overseer` | Verify its gates, child counts, and PR verdicts against current artifacts, then resume the applicable numbered lifecycle step. It is a current-state snapshot, not a raw-event replay. For each entry in its `phaseCompletions` (`{issue, role, summary, at}`, phases that completed while you were not live), handle it exactly as a `phase-complete` wake. |
| `worker-died` | Payload `{type:"worker-died", issue, role}`. The daemon probed and retried this role's worker through `MAX_LAUNCH_FAILURES` attempts and could not confirm a boot — never a raw-event replay or a silent revive. Reassess the work and `spawn_worker` again for the role (it resumes the same agent via `--resume` if a session file survived) or reassign it if the failure looks environmental, not agent-specific. |
| `reopened` | Reopen the root lifecycle: inspect the reason and current artifacts, reassess scope and children, and resume at the first applicable numbered step. |

## Escalation judgment

Controller-actionable matters are exactly re-filing a genuinely independent child,
capacity, and cross-tree conflict. Use the Legion escalation operation for those. Handle
everything else in the tree, or use `dispatch_ask` for a human question; workers may reach
Sami directly with `dispatch_ask` the same way. Do not create a wait loop for any wake
source.
