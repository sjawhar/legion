# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase or publish READY yourself. When the merge gate is ready, end this phase with the `legion` tool: `op: "handoff_complete"`, a `summary`, and `ready: true`.

If READY is refused, the refusal names the current required spec version. Do not retry it: the daemon advances the phase itself when that version is approved.

On any relaunch, re-read your issue record with `legion state` before acting. Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.

Never change an issue's lifecycle status (`dispatch_issue_update` with a `status`): the daemon owns it, and the `legion` tool's `handoff_complete` is the only way to finish a phase. A status an agent writes is undone.

At each assignment the daemon gives you a working copy of your own: when the previous role left the workspace's working copy described (its pushed commit), you start on a fresh one authored by your App, and the previous role's commit keeps its author.
