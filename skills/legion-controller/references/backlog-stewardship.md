# Backlog stewardship

The controller keeps the Dispatch backlog's facts aligned with the deployment's current weekly
plan. The plan is the source of the work order and its one-owner map; Dispatch priority and board
rank are the executable form of that order. The deployment instructions name the weekly-planning
issue and its `plan-week-*` artifact.

## Controller routing address

The controller keeps its startup role, `legion-<project>-controller`; it never claims
`backlog-order`, which is retired. Send READY packets, scope questions, and routing requests to
`notifications.role.legion-<project>-controller`. Do not add another role claim or manual
subscription: changing roles ends the controller's wake delivery.

## Reconcile the weekly plan

Run this pass on the first turn of every UTC day and after every ruling from Sami:

1. Read the weekly-planning issue and select its newest `plan-week-*` artifact by its latest
   `artifact.version` event. Read that artifact before changing any issue.
2. For every current plan line in this deployment's project, make Dispatch's sort order exactly
   match the line order: set the coarse priority and board rank together so the admission rule
   (highest priority, then board rank) produces the plan's order. Keep each line's own stated
   sequence. Do not invent a second order from a title, a stale route, or an instruction.
   Reorder through Dispatch's board-rank API described in
   `skill://dispatch/references/issues.md#board-rank-priority-and-assignee-on-a-read`.
3. Reconcile the plan's one-owner map. For each listed owner, verify the named session is live.
   For unowned work, find a suitable live owner with `envoy_sessions` filtered by `title` or `dir`,
   route the issue to that exact session, and tell it what it owns. Do not change the human
   `Assignee:` field: it controls who receives asks, not the working session.
4. Rewrite the plan row with `dispatch doc-edit` guarded by the document token from the current
   `dispatch doc-read`. The artifact states the current rule; replace superseded text instead of
   keeping a change log in it. A comment or message does not update the one-owner map.

A ruling from Sami is a direct Dispatch message to this controller. Answer it first, carry out its
scope or priority decision, then record the resulting current rule in the affected weekly-plan row
(or its ruling section when it has no one line). Link the message when Dispatch gave it a reference.
A ruling triggers the full reconciliation pass even when it appears to affect only one issue.

## Answering controller deliveries

Read every delivery to `notifications.role.legion-<project>-controller` against live Dispatch and
daemon state. Answer scope questions from the weekly plan and deployment instructions. Route a
request to the verified owner when it is not the controller's decision.

Until the deployment's daemon arms READY pull requests, answer each READY packet with its current
queue or hold outcome and route it to the live owner named by the deployment's rules. Never merge
or use a GitHub credential from the controller. Once the daemon arms READY pull requests, the
controller confirms the Dispatch record and leaves the arm to the daemon.

## Recovering stopped-session work

Treat a session route as stopped only after the owner audit's two `no_holder` reads at least ten
minutes apart. Do not poll to create the second read; use the next normal controller wake. Confirm
that no live owner is available with `envoy_sessions` before changing the issue.

For work that the plan or its handoff says moves to Legion when its session stops, preserve its
existing labels, add `legion`, then use `legion status <KEY> todo`. The label comes first: it is what
lets the daemon admit the root. Do not apply that recovery to an issue already in an active Legion
tree; its architect and daemon own its lifecycle. Update the weekly-plan owner row after the move.
