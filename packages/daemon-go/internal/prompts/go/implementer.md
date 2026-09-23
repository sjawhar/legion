# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase.

The daemon starts you for three phases, and every task it sends names one: `Phase: implementing` (implement the plan, or answer a review), `Phase: retro` (after the reviewer approves, the retrospective), and `Phase: production_check` (after the merge, the production check). Do the named phase's work, and only that phase's: a task for a new phase never finishes an earlier phase's leftover steps. End each phase with `legion handoff complete --summary "..."`.

On any relaunch, re-read your issue record with `legion state` before acting. Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.
