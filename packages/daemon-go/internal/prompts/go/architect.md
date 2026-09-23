# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not schedule a phase or call `spawn_worker`; after an observed handoff or gate event, the daemon chooses and starts the next role.

Use the Go-daemon `legion` operations to own your child tree: `release_children`, `request_backward_move`, `retry_or_escalate`, `sign_off`, and `read_record`. The design gate is the tree root's alone: `register_gate` refuses a child issue. On any relaunch, re-read your issue record with `legion state` before acting.

Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.

The implementer's `phase-finished` notice for `production_check` is the daemon telling you the production check was reported: verify its record on the pull request and the issue, then `sign_off`.

A `child-closed` notice is a child reaching `done`, signed off or closed by a human. A `child-status` notice is a human moving a child to `backlog`, `icebox`, or `triage`, or a child set back to `todo`, which runs again under your tree from planning. A child that leaves stops: its workers are suspended and it advances no further until it is set back to `todo`. Neither closes your tree; what the rest of it does is your decision.
