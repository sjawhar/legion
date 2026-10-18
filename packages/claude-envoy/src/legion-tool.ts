import type { LegionRole } from "@legion/contracts"
import type { LegionPhase } from "@legion/contracts/legion-api"
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client"
import {
  type ClaimCapability,
  executePhaseOp,
  legionToolDescription,
  PHASE_OP_SENTENCES,
  PHASE_OPS,
  phaseOpParameters,
} from "@legion/pi-shared/legion-phase-ops"
import { z } from "zod"

/** The tool's name: Claude Code shows it as `mcp__legion__legion`, under the pane's one server. */
export const LEGION_TOOL_NAME = "legion"

/** The tool as `tools/list` declares it: one object schema, `op` choosing one of the phase ops
 * (`@legion/pi-shared/legion-phase-ops`), the operations a phase worker gets. */
export function legionToolDefinition() {
  return {
    name: LEGION_TOOL_NAME,
    description: legionToolDescription({
      envoyPublish: "mcp__legion__envoy_publish",
      before: PHASE_OP_SENTENCES.readRecord,
      after: PHASE_OP_SENTENCES.requestBackwardMove,
    }),
    inputSchema: z.toJSONSchema(z.object({ op: z.enum(PHASE_OPS), ...phaseOpParameters(z) })),
  }
}

export interface LegionToolRuntime {
  readonly daemon: LegionDaemonClient
  /** The pane's registered claim, or undefined while its boot is still registering. */
  readonly claim: () => ClaimCapability | undefined
  /** The commit a completion of `phase` by `role` reports (`findHandoffCommit` with the pane's
   * environment): throws naming the remedy when the handoff is not committed and pushed. */
  readonly handoffCommit: (phase: LegionPhase, role: LegionRole) => Promise<string>
}

/** Runs one call of the `legion` tool and answers its JSON result; a refusal throws its reason. */
export async function executeLegionTool(
  runtime: LegionToolRuntime,
  input: unknown,
): Promise<unknown> {
  const claim = runtime.claim()
  if (claim === undefined) {
    throw new Error(
      "this session has no Legion claim yet: the pane registers with the daemon once its first turn starts",
    )
  }
  return executePhaseOp(
    { daemon: runtime.daemon, claim, handoffCommit: runtime.handoffCommit },
    input,
  )
}
