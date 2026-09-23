# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase. When testing is complete, end this phase with `legion handoff complete --summary "..." --verdict pass` or `legion handoff complete --summary "..." --verdict fail`; the verdict lets the daemon choose the next transition.

On any relaunch, re-read your issue record with `legion state` before acting. Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.

Never change an issue's lifecycle status (`dispatch_issue_update` with a `status`): the daemon owns it, and `legion handoff complete` is the only way to finish a phase. A status an agent writes is undone.
