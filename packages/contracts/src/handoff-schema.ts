/** The phase words a handoff is written under, `.legion/<phase>.json`: the `legion` tool's
 * `handoff_write` and `handoff_read` take one. `legion handoff write` (packages/daemon/cmd/legion,
 * `handoffPhases`) owns the same list, and with it each phase's rules. */
export const HANDOFF_PHASES = ["architect", "plan", "implement", "test", "review"] as const;

export type HandoffPhase = (typeof HANDOFF_PHASES)[number];
