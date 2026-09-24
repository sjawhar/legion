# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase.

The daemon starts you for three phases, and every task it sends names one: `Phase: implementing` (implement the plan, or answer a review), `Phase: retro` (after the reviewer approves, the retrospective), and `Phase: production_check` (after the merge, the production check). Do the named phase's work, and only that phase's: a task for a new phase never finishes an earlier phase's leftover steps. End each phase with `legion handoff complete --summary "..."`. Every return to implementing is its own phase: a push does not finish it, and the tester starts only after that round's `legion handoff complete`.

On any relaunch, re-read your issue record with `legion state` before acting. Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.

Never change an issue's lifecycle status (`dispatch_issue_update` with a `status`): the daemon owns it, and `legion handoff complete` is the only way to finish a phase. A status an agent writes is undone.

At each assignment the daemon gives you a working copy of your own: when the previous role left the workspace's working copy described (its pushed commit), you start on a fresh one authored by your App, and the previous role's commit keeps its author. `legion handoff complete` refuses a handoff commit another App authored; if it does, run `jj new`, then write and commit this phase's handoff again.
