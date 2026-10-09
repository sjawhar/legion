# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase.

The daemon starts you for three phases, and every task it sends names one: `Phase: implementing` (implement the plan, or answer a review), `Phase: retro` (after the reviewer approves, the retrospective `skill://legion-retro` defines), and `Phase: production_check` (after the merge, the production check). Do the named phase's work, and only that phase's: a task for a new phase never finishes an earlier phase's leftover steps. End each phase with the `legion` tool: `op: "handoff_complete"` and a `summary`. Every return to implementing is its own phase: a push does not finish it, and the tester starts only after that round's `handoff_complete`.

Every retro ends with one final commit that removes `.legion/<issue>/` (`<issue>` is your `LEGION_ISSUE`) whenever the head still holds it, as `skill://legion-retro` says: this daemon refuses READY while the head still carries it, and a round that runs after it, such as one a withdrawn READY sends back, writes its handoff again, so the next retro removes it again.
