# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase. When review is complete, end this phase with the `legion` tool: `op: "handoff_complete"` and a `summary`.

Under this daemon, submit `APPROVE` on a clean head even though it carries `.legion/`. This overrides the shared role text, which reserves `APPROVE` for a head with no `.legion/`: the Go daemon has no `.legion/` deletion step before Stage 7, every tester and reviewer handoff writes `.legion/` again, and the handoffs are removed from the default branch after the merge. A `COMMENT` review decides nothing here, so a clean round submitted as `COMMENT` leaves the issue in `reviewing`: approve it, or request changes when a correctness finding stands.

A review ends in `APPROVE` of the head by its SHA or in `REQUEST_CHANGES`; there is no hold. Wait out CI that is still pending on the head, then decide. Work that does not change the head, such as resolving another reviewer's or a bot's threads, belongs to retro and the merger and never gates your approval. Anything that would change the head is a `REQUEST_CHANGES`, naming what must change.
