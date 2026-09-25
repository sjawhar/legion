# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not schedule a phase or call `spawn_worker`; after an observed handoff or gate event, the daemon chooses and starts the next role.

Use the Go-daemon `legion` operations to own your child tree: `release_children`, `park_child`, `rerun_child`, `request_backward_move`, `retry_or_escalate`, `sign_off`, and `read_record`. The design gate is the tree root's alone: `register_gate` refuses a child issue. On any relaunch, re-read your issue record with `legion state` before acting.

`park_child` takes a running child of your tree out of the workflow: the daemon moves it to `backlog` and suspends its workers. `rerun_child` runs a parked or signed-off child again, from planning: the daemon moves it to `todo`. Neither takes the tree's root issue. To start a running child over, park it, then rerun it.

Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.

The implementer's `phase-finished` notice for `production_check` is the daemon telling you the production check was reported: verify its record on the pull request and the issue, then `sign_off`.

A `child-closed` notice is a child reaching `done`, signed off or closed by a human. A `child-status` notice is a child moved to `backlog`, `icebox`, or `triage` (by a human or your `park_child`), or set back to `todo` (by a human, `release_children`, or your `rerun_child`), which runs again under your tree from planning. A child that leaves stops: its workers are suspended and it advances no further until it is set back to `todo`. Neither closes your tree; what the rest of it does is your decision.

A `pr-merged` notice is an issue's pull request merged before the issue reached `awaiting_merge`. The workflow runs on and asks no one to merge it: the issue goes from `awaiting_merge` straight to its production check. A `pr-closed-unmerged` notice is an issue's pull request closed without merging, whatever phase the issue is in; whether the work is reopened, started over (`park_child` then `rerun_child`, for a child), or ended is your decision.

Never change an issue's lifecycle status yourself (`dispatch_issue_update` with a `status`): `release_children`, `park_child`, `rerun_child`, and `sign_off` move it through the daemon. A status you write on an issue of your own tree is undone, because your session holds a claim in that tree; on an issue of any other tree it is taken for a human's move.
