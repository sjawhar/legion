# Backlog stewardship

The controller keeps the Dispatch backlog's facts aligned with the deployment's current weekly
plan. The plan is the source of the work order and its one-owner map; Dispatch board rank is the
executable form of that order. The deployment instructions name the weekly-planning issue and its
`plan-week-*` artifact.

## Controller routing address

Claim no Envoy role other than `legion-<project>-controller`, your startup role, because changing
roles ends the controller's wake delivery. READY packets, scope questions and routing requests
reach you on `notifications.role.legion-<project>-controller`.

## Reconcile the weekly plan

Run this pass on the first turn of every UTC day and after every ruling from a person:

1. Read the weekly-planning issue and select its newest `plan-week-*` artifact by its latest
   `artifact.version` event. Read that artifact before changing any issue.
2. For every plan line whose issue key carries this project's prefix (`daemon.project` in
   `read_state`), make the project's board order match the line order exactly, because Legion
   admits waiting roots by board rank alone. Set the priority the plan line states, and leave the
   priority where it states none; priority changes no admission order. When one plan line lists
   several issues, keep them in that order. Take the order from the plan alone, never from a
   title or a route. Reorder with
   `dispatch issue-update --issue <KEY> --before <OTHER>` or `--after <OTHER>`.
3. Reconcile the plan's one-owner map. A session is live when `envoy_sessions` lists it; check
   each listed owner that way. For unowned work, find an owner whose `envoy_sessions` `dir` is a
   checkout of the issue's repository, route the issue to it
   (`dispatch issue-update --issue <KEY> --route session:<session id>`) and `envoy_send` it
   `You own <KEY>: <title>.`. Routing starts nothing on Legion and sets no label. Never route an
   issue that carries the `legion` label, because Legion's own roles work it. Do not change the
   human `Assignee:` field: it decides who receives asks, not who works the issue.
4. Edit that issue's line in the plan artifact (its owner column) with `dispatch doc-edit`,
   guarded by the document token from the current `dispatch doc-read`. Replace superseded text
   instead of keeping a change log in it, because the artifact states the current rule. A comment
   or message does not update the one-owner map.

A ruling from a person is a direct Dispatch message to this controller. Answer it first, carry out
its scope or priority decision, then record the resulting current rule in the affected weekly-plan
row (or its ruling section when it has no one line). Link the message when Dispatch gave it a
reference.
A ruling triggers the full reconciliation pass even when it appears to affect only one issue.

## Answering controller deliveries

Read every delivery to `notifications.role.legion-<project>-controller` against live Dispatch and
daemon state. You decide labels, the statuses `todo`, `backlog` and `icebox`, and board order.
Answer a scope question from the weekly plan and the deployment instructions. Anything else goes
with `envoy_send` to the session the plan's one-owner map names for that issue.

For a READY packet, confirm the Dispatch record and leave the merge to the daemon. Never merge or
use a GitHub credential to merge from the controller, because the daemon submits the merge itself
at READY's head.

## Recovering stopped-session work

Treat a session route as stopped only after two reads of
`dispatch issues --project <KEY> --route-status no_holder --limit 250` at least ten minutes apart
both list it, as "The owner audit" in `skill://dispatch/references/issues.md` says, because a
restarting session is absent for minutes. Do not poll to create the second read; use the next
controller wake. Then check that `envoy_sessions` lists no session whose `dir` is a checkout of
the issue's repository.

For work that a ruling of a person's, recorded in the plan with its link (the ruling section
above), says moves to Legion when its session stops, preserve its existing labels, add `legion`,
then set it `todo` with the `legion` tool's `set_status`. A plan row you wrote with no such ruling
behind it moves nothing. The label comes first: it is what
lets the daemon admit the root. Do not apply that recovery to an issue already in an active Legion
tree; its architect and daemon own its lifecycle. Update the weekly-plan owner row after the move.
