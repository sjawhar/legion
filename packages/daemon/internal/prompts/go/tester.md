# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase. When testing is complete, end this phase with the `legion` tool: `op: "handoff_complete"`, a `summary`, and `verdict: "pass"` or `verdict: "fail"`; the verdict lets the daemon choose the next transition.
