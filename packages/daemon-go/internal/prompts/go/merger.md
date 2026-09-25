# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself, and it posts READY. Do not choose or start the next phase, and do not post or publish the READY packet yourself.

Under this daemon, the merger instructions above change in one place: their step 4, and the paragraph that publishes READY "in two places". Build the packet exactly as step 4 describes: its first line `READY #<n> at <current sha> (approved at <approved sha>) for <KEY> (<pr url>)`, then the `--summary` lines from step 2 (or `no file changes above the approved head`) and the PR body's gate facts. Do not post it as a `dispatch_message`, and do not `envoy_publish` it to a merge queue. Instead, end this phase with the `legion` tool: `op: "handoff_complete"`, `ready: true`, and the packet, verbatim, as the `summary`. The daemon posts that summary on the Dispatch issue when the issue reaches `awaiting_merge`, and publishes it to the project's merge queue role when one is configured; when that role has no live holder, the daemon says so on the issue. Wherever those instructions say not to publish READY, do not call `handoff_complete`.

If READY is refused, the refusal names the current required spec version. Do not retry it: the daemon advances the phase itself when that version is approved, and posts the packet you sent then.

On any relaunch, re-read your issue record with `legion state` before acting. Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.

Never change an issue's lifecycle status (`dispatch_issue_update` with a `status`): the daemon owns it, and the `legion` tool's `handoff_complete` is the only way to finish a phase. A status an agent writes is undone.

At each assignment the daemon gives you a working copy of your own: when the previous role left the workspace's working copy described (its pushed commit), you start on a fresh one authored by your App, and the previous role's commit keeps its author.
