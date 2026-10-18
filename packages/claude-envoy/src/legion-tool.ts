import type { LegionRole } from "@legion/contracts"
import {
  LEGION_PHASE_BACKWARD_TARGETS,
  type LegionIssue,
  type LegionPhase,
  type LegionState,
} from "@legion/contracts/legion-api"
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client"
import { z } from "zod"
import type { LegionClaimCapability } from "./legion-claim"

/** The tool's name: Claude Code shows it as `mcp__legion__legion`, under the pane's one server. */
export const LEGION_TOOL_NAME = "legion"

/**
 * A phase worker's operations on the daemon's role-local workflow surface, the subset of
 * pi-legion's `legion` tool (`packages/pi-legion/src/tools.ts`) a phase worker gets there: read an
 * issue's record, complete the phase, ask for a backward move. Every write mints its grant with the
 * claim's secret and posts it with the request; nothing is written to the pane.
 */
const LegionToolArguments = z.discriminatedUnion("op", [
  z.strictObject({ op: z.literal("read_record"), issue: z.string().trim().min(1) }),
  z.strictObject({
    op: z.literal("handoff_complete"),
    summary: z.string().trim().min(1),
    verdict: z.enum(["pass", "fail"]).optional(),
    ready: z.boolean().optional(),
  }),
  z.strictObject({
    op: z.literal("request_backward_move"),
    to: z.enum(LEGION_PHASE_BACKWARD_TARGETS),
    reason: z.string().trim().min(1),
  }),
])

const DESCRIPTION =
  "Perform the workflow operation the Legion daemon assigned this role. The daemon advances phases; this tool cannot spawn workers. " +
  "read_record returns an issue's record from the daemon's state. " +
  "handoff_complete reports this phase complete (the daemon accepts a completion only from the role working the issue's current phase): " +
  "`summary` (two sentences for the architect, or the merger's READY packet), `verdict` pass|fail when your role's instructions require one, " +
  "`ready: true` for the merger's READY. A handoff is a committed file, `.legion/<issue>/<phase>.json`: the pushed commit carrying it is found " +
  "here, with the pane's jj, and reported, so write it, commit it and push the branch (`jj git push`) before calling; this call refuses, posting " +
  "nothing, a handoff still in the working copy, missing, not committed on this branch, carried by another pane's commit, or not yet on " +
  "`legion/<issue>@origin`, each naming the remedy. The daemon refuses HANDOFF_NOT_NEW (the commit is the one you reported for your previous " +
  "phase: write and commit this phase's handoff) and, for READY, each refusal the merger's role names; a refused completion changed nothing, " +
  "so fix what it names and complete again. An accepted READY whose `note` begins MANUAL_MERGE_REQUIRED: is one a person merges: arm nothing. " +
  "request_backward_move asks the daemon to move this issue back to an earlier phase, `to`, for `reason`. " +
  "What a later phase needs goes in your handoff; a question for another live role goes to its role topic with envoy_publish."

/** The tool as `tools/list` declares it: one object schema, `op` choosing the operation. */
export function legionToolDefinition() {
  return {
    name: LEGION_TOOL_NAME,
    description: DESCRIPTION,
    inputSchema: z.toJSONSchema(
      z.object({
        op: z.enum(["read_record", "handoff_complete", "request_backward_move"]),
        issue: z.string().describe("read_record's issue key").optional(),
        summary: z.string().optional(),
        verdict: z.enum(["pass", "fail"]).optional(),
        ready: z.boolean().optional(),
        to: z.enum(LEGION_PHASE_BACKWARD_TARGETS).optional(),
        reason: z.string().optional(),
      }),
    ),
  }
}

function recordFrom(state: LegionState, issue: string): LegionIssue {
  const record = state.issues[issue]
  if (record === undefined) throw new Error(`The daemon has no record for ${issue}`)
  return record
}

export interface LegionToolRuntime {
  readonly daemon: LegionDaemonClient
  /** The pane's registered claim, or undefined while its boot is still registering. */
  readonly claim: () => LegionClaimCapability | undefined
  /** The commit a completion of `phase` by `role` reports (`findHandoffCommit` with the pane's
   * environment): throws naming the remedy when the handoff is not committed and pushed. */
  readonly handoffCommit: (phase: LegionPhase, role: LegionRole) => Promise<string>
}

/** Runs one call of the `legion` tool and answers its JSON result; a refusal throws its reason. */
export async function executeLegionTool(
  runtime: LegionToolRuntime,
  input: unknown,
): Promise<unknown> {
  const active = runtime.claim()
  if (active === undefined) {
    throw new Error(
      "this session has no Legion claim yet: the pane registers with the daemon once its first turn starts",
    )
  }
  const args = LegionToolArguments.parse(input)
  const { daemon } = runtime
  const grant = async (): Promise<string> =>
    (
      await daemon.grant({
        sessionId: active.sessionID,
        secret: active.secret,
        tree: active.tree,
        issue: active.issue,
      })
    ).grantId
  switch (args.op) {
    case "read_record":
      return { record: recordFrom(await daemon.state(), args.issue) }
    case "handoff_complete": {
      if (active.role === "architect" && active.issue === active.tree) {
        throw new Error(
          "handoff_complete is not available to a root architect session, which runs no phase",
        )
      }
      // The commit is found before the grant is minted: a refusal here posts nothing and mints
      // nothing. The phase is the issue record's, the role the claim's.
      const { phase } = recordFrom(await daemon.state(), active.issue)
      const commit = await runtime.handoffCommit(phase, active.role)
      return daemon.handoffComplete({
        grantId: await grant(),
        summary: args.summary,
        verdict: args.verdict ?? "",
        ready: args.ready ?? false,
        commit,
      })
    }
    case "request_backward_move":
      await daemon.phaseBackward({ grantId: await grant(), to: args.to, reason: args.reason })
      return {}
  }
}
