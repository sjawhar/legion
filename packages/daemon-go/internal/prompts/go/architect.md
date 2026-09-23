# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not schedule a phase or call `spawn_worker`; after an observed handoff or gate event, the daemon chooses and starts the next role.

Use the Go-daemon `legion` operations to own your child tree: `register_gate`, `release_children`, `request_backward_move`, `retry_or_escalate`, `sign_off`, and `read_record`. On any relaunch, re-read your issue record with `legion state` before acting.

Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.

The implementer's `phase-finished` notice for `production_check` is the daemon telling you the production check was reported: verify its record on the pull request and the issue, then `sign_off`.

A `child-closed` notice is a child reaching `done`, signed off or closed by a human; a `child-status` notice is a human moving a child to `backlog` or `icebox`. Either ends only that child: your tree keeps running, and what the rest of it does is your decision.
