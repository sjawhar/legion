---
name: legion-controller
description: Use when handling Legion controller wakes for root-issue triage, backlog admission, architect escalation, resync healing, or human interaction.
---

# Legion Controller

The controller is the one persistent, wake-driven session for a Legion project. It makes
triage, escalation, and human-interaction judgments; it never does phase-worker work or
routes raw events into an architect.

This skill documents the target Dispatch-native contract. The `legion` tool's `set_status`
op, `dispatch_read`/`dispatch_issue`, and the Dispatch key format land with PR B (#TBD);
until that PR merges, this skill's contract is not yet runnable on `main`.

## Start and claim the controller role

The Legion extension claims `legion-<project>-controller` and registers controller readiness
with the daemon during session startup. Do not handle a wake unless that startup succeeded.

For an interactive takeover, start OMP with `LEGION_CONTROLLER_SECRET` and
`LEGION_DAEMON_URL` in its environment, then run:

```text
/legion-claim-controller
```

The command resolves the project from daemon state, claims the Envoy role for the current
session, and posts readiness before controller commands can act. It retains the environment
capability for `legion({ op: "set_status", issue, status })`. Never pass a secret as a command argument
or copy it into a transcript.

This handshake lets the daemon redeliver held controller work. It does not turn the controller
into a state holder: daemon state and the Dispatch project remain authoritative.

## Turn discipline

- **Direct user message always first.** If this turn includes a direct user message, answer
  it before handling every other wake.
- **One wake = one turn.** Handle exactly the wake's implication, then end the turn. Never
  poll, idle-loop, or wait for another event.
- **Wakes are advisory.** Before any side effect, verify the current daemon state and the
  relevant Dispatch issue. A stale or duplicate wake may cost a read, never a wrong action.
- **Controller state is disposable.** Do not reconstruct or preserve local controller
  bookkeeping between turns.

## Wake routing table

| Wake | Content | Controller action |
|---|---|---|
| New issue created in the Dispatch project (`issue.created`, status `triage`; resync heals misses) | issue key + triage context (incl. pre-existing children) | Triage: `legion({ op: "set_status", issue, status: "todo" })` to admit, or set `backlog`/`icebox` to park |
| Backlog eligibility | slot freed / priority change | Reconsider parked items and move the eligible root to `todo` |
| Architect escalation (controller-actionable only: re-file a child as a root issue, capacity, cross-tree conflicts) | request + context | Judge and act; issue-scoped human Q&A goes through `dispatch_ask` from the owning architect, not here |
| Resync report | artifact-driven anomaly list (zero-owner trees, untriaged-open, launch-failed) | Verify against fresh state, then heal |
| `child-status` | child key + status transition | Not controller-actionable by default; if the daemon could not route it to the parent's architect role, verify the transition and forward it with `envoy_publish` |
| Mention | Slack/GitHub PR @mention text | Answer, or route to the owning issue's architect role |
| Closed-tree activity (comment, review, CI on a closed tree) | issue, root, event summary | Read the artifact; if work should resume, `legion({ op: "set_status", issue: root, status: "todo" })`; otherwise no action — the event is not held or redelivered |
| Direct user message | — | Always first |

## New issue triage

1. Read `legion state --json`, then inspect the reported Dispatch issue with `dispatch_read`.
   Verify the issue is in this project, is eligible for a root process, and whether it
   has pre-existing children. Dispatch and daemon state, not the wake text, decide triage.
2. If it should run now, admit the root issue:

   ```text
   legion({ op: "set_status", issue: "<issue>", status: "todo" })
   ```

3. If it should deliberately wait, move it to a parked status instead of leaving it in
   `triage`:

   ```text
   legion({ op: "set_status", issue: "<issue>", status: "backlog" })
   ```

   (or `status: "icebox"` for longer-term deferral). Dispatch status is the durable record;
   there is no separate marker to maintain. Do not triage a system-created child as a root
   issue.

## Backlog eligibility

When a slot frees or priority changes, use `legion state --json` and the current Dispatch
issue to reconsider parked roots. Admit the selected root with
`legion({ op: "set_status", issue, status: "todo" })`. Moving an item to or from `backlog`/
`icebox` is a deliberate controller decision, not a no-op.

## Architect escalation

Only decide controller-actionable escalations: re-filing independent work, capacity, and
cross-tree conflicts. Issue-scoped human Q&A goes through `dispatch_ask` from the owning
architect, not the controller.

For an independence judgment, verify the child and its parent against current daemon state
and the Dispatch issue. If the work belongs in an independent root:

1. File a **fresh root issue** with `dispatch_issue({ project, title, spec })` (no `parent`).
   `project` is the issue key's prefix before `-<n>` (e.g. `LEGSMOKE-3` → `LEGSMOKE`) — not
   the role-token `<project>` (the daemon's own project, e.g. `acme`), a different string.
2. Park the child (`legion({ op: "set_status", issue: child, status: "icebox" })`) and leave
   a pointer to the new root issue. The controller's capability is `todo`/`backlog`/`icebox`
   only — only the owning architect or the daemon closes an issue as `done`.
3. Admit or deliberately backlog the new root through the normal triage procedure.

Never promote a child in place. Resolve capacity and cross-tree conflicts from verified
state, routing design decisions back to the owning architect when they are not controller
judgments.

## Resync report

Treat a resync report as an anomaly list, not an instruction. For every zero-owner tree,
untriaged-open, or launch-failed issue it names, verify `legion state --json` and the
current Dispatch issue first. Then heal the verified condition: admit an eligible root, move
an issue back to its intended status, or use the applicable daemon control path. Do not act
on stale entries until their source artifact explains the anomaly.

## Mentions

Read the mention and its artifact. Answer it when it asks the controller for triage or
human-facing information. Otherwise resolve the authoritative owning architect role and
route the verified context with `envoy_publish`. Do not route raw event traffic or invent a
role token from a partial issue reference.

