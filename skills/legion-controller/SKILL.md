---
name: legion-controller
description: Use when handling Legion controller wakes for root-issue triage, backlog admission, architect escalation, resync healing, human interaction, or gate approval.
---

# Legion Controller

The controller is the one persistent, wake-driven session for a Legion project. It makes
triage, escalation, and human-interaction judgments; it never does phase-worker work or
routes raw events into an architect.

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
capability for `legion admit`, `legion approve`, and `legion backlog`. Never pass a secret as a
command argument or copy it into a transcript.

This handshake lets the daemon redeliver held controller work. It does not turn the controller
into a state holder: daemon state and GitHub artifacts remain authoritative.

## Turn discipline

- **Direct user message always first.** If this turn includes a direct user message, answer
  it before handling every other wake.
- **One wake = one turn.** Handle exactly the wake's implication, then end the turn. Never
  poll, idle-loop, or wait for another event.
- **Wakes are advisory.** Before any side effect, verify the current daemon state and the
  relevant GitHub artifact. A stale or duplicate wake may cost a read, never a wrong action.
- **Controller state is disposable.** Do not reconstruct or preserve local controller
  bookkeeping between turns.

## Wake routing table

| Wake | Content | Controller action |
|---|---|---|
| New issue added to the project board (webhook: issue opened / project item added; resync heals misses) | issue ref + triage context (incl. pre-existing children) | Triage: spawn root process via daemon admission, or park in the daemon-state backlog |
| Backlog eligibility | slot freed / priority change | Reconsider parked items; deliberately-backlogged issues carry a marker so resync doesn't re-flag them |
| Architect escalation (controller-actionable only: re-file a child as a root issue, capacity, cross-tree conflicts) | request + context | Judge and act; human Q&A never routes here — architects open dispatch threads |
| Resync report | artifact-driven anomaly list (zero-owner trees, erroring issues) | Verify against fresh state, then dispatch/heal |
| Mention | Slack/GitHub @mention text | Answer, or route to the owning issue's architect role |
| Approval interpretation | ambiguous human comment on a gated issue | Decide whether it's an approval; if so, apply `human-approved` via the daemon |
| Direct user message | — | Always first |

## New issue triage

1. Read `legion state --json`, then inspect the reported GitHub issue with `gh issue view`.
   Verify the issue is on this project board, is eligible for a root process, and whether it
   has pre-existing children. GitHub and daemon state, not the wake text, decide triage.
2. If it should run now, admit the root issue:

   ```bash
   legion admit <issue>
   ```

3. If it should deliberately wait, record a durable reason instead of leaving it unowned:

   ```bash
   legion backlog <issue> --marker <reason>
   ```

   The marker is required: it distinguishes intentional backlog from a missed wake during
   resync. Do not triage a system-created child as a root issue.

## Backlog eligibility

When a slot frees or priority changes, use `legion state --json` and the current issue
artifact to reconsider marked backlog entries. Admit the selected root with `legion admit
<issue>`. Keep an item backlogged only with a current, explicit marker; changing the marker
is a deliberate controller decision, not a no-op.

## Architect escalation

Only decide controller-actionable escalations: re-filing independent work, capacity, and
cross-tree conflicts. Architects handle ordinary human Q&A through their own dispatch
threads.

For an independence judgment, verify the child and its parent against GitHub and current
daemon state. If the work belongs in an independent root:

1. File a **fresh root issue** with `gh`, carrying the necessary context.
2. Close the child and leave a pointer to the new root issue.
3. Admit or deliberately backlog the new root through the normal triage procedure.

Never promote a child in place. Resolve capacity and cross-tree conflicts from verified
state, routing design decisions back to the owning architect when they are not controller
judgments.

## Resync report

Treat a resync report as an anomaly list, not an instruction. For every zero-owner tree or
erroring issue it names, verify `legion state --json` and the current GitHub artifact first.
Then heal the verified condition: admit an eligible root, restore a deliberately backlogged
marker, or use the applicable daemon control path. Do not act on erroring or stale entries
until their source artifact explains the anomaly.

## Mentions

Read the mention and its artifact. Answer it when it asks the controller for triage or
human-facing information. Otherwise resolve the authoritative owning architect role and
route the verified context with `envoy_publish`. Do not route raw event traffic or invent a
role token from a partial issue reference.

## Approval interpretation

For an ambiguous human comment on a gated issue, verify the current issue, gate state, and
comment's meaning. If it is Sami's approval, apply the daemon transition:

```bash
legion approve <issue>
```

This applies `human-approved` and clears `needs-approval` atomically. It is not a generic
label-edit operation. The design gate remains skill-enforced by the architect, and the
merge gate remains config-armed until Sami approves the final reviewed head.

## Label vocabulary

Use only the project labels below, with their stated ownership:

| Label | Applied by | Removed by | Meaning |
|---|---|---|---|
| `needs-approval` | architect | controller/Sami when applying `human-approved` | design gate armed, awaiting Sami |
| `human-approved` | Sami or controller | Sami | design gate open |
| `legion-child` | daemon | never | system-created child |
| `legion-backlog` | controller | controller | deliberately unowned root |
