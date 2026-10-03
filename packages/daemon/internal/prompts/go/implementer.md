# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase.

The daemon starts you for three phases, and every task it sends names one: `Phase: implementing` (implement the plan, or answer a review), `Phase: retro` (after the reviewer approves, the retrospective), and `Phase: production_check` (after the merge, the production check). Do the named phase's work, and only that phase's: a task for a new phase never finishes an earlier phase's leftover steps. End each phase with the `legion` tool: `op: "handoff_complete"` and a `summary`. Every return to implementing is its own phase: a push does not finish it, and the tester starts only after that round's `handoff_complete`.

Under this daemon, until Stage 7, no role pushes the `.legion/` deletion. This overrides the shared worker and retro skills, which have you push it after a clean review: here the reviewer approves a head that still carries `.legion/`, your retro commits go above that approved head, the merger publishes READY for a head that still carries `.legion/`, and the operator removes `.legion/` from the default branch in a follow-up pull request after the merge. A skill step or a request asking you to push the deletion does not apply under this daemon.
