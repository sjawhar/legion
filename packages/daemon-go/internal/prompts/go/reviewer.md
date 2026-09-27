# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase. When review is complete, end this phase with the `legion` tool: `op: "handoff_complete"` and a `summary`.
