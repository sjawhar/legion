import { LEGION_GO_PHASES, type LegionGoState } from "@legion/contracts/legion-go-api";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "../pi-types";
import { toolFailure, toolSuccess } from "../tool-result";
import type { LegionGoDaemonClient } from "./go-daemon-client";
import {
  HANDOFF_DESCRIPTION,
  HANDOFF_OPERATIONS,
  handoffSchemaFields,
  isHandoffOperation,
  runHandoffAction,
} from "./handoff-actions";

export type GoLegionToolRole = "architect" | "phase-worker";

export interface GoLegionToolSession {
  readonly kind: GoLegionToolRole;
  readonly sessionId: string;
  readonly tree: string;
  readonly issue: string;
  readonly secret?: string;
}

const OPERATIONS: Readonly<Record<GoLegionToolRole, readonly string[]>> = {
  architect: [
    "register_gate",
    "release_children",
    "request_backward_move",
    "retry_or_escalate",
    "sign_off",
    "read_record",
  ],
  "phase-worker": ["request_backward_move", "read_record"],
};

const OPERATION_FIELDS: Readonly<Record<string, readonly string[]>> = {
  register_gate: ["issue", "artifactId", "version"],
  release_children: ["issues"],
  request_backward_move: ["to", "reason"],
  retry_or_escalate: ["issue", "decision"],
  sign_off: ["issue"],
  read_record: ["issue"],
};

const jsonSuccess = (details: Readonly<Record<string, unknown>>): ToolResult =>
  toolSuccess(JSON.stringify(details), details);

function toolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  return z.object({
    op: z.enum([
      "register_gate",
      "release_children",
      "request_backward_move",
      "retry_or_escalate",
      "sign_off",
      "read_record",
      ...HANDOFF_OPERATIONS,
    ]),
    issue: z.string().optional(),
    artifactId: z.string().optional(),
    version: z.number().optional(),
    issues: z.array(z.string()).optional(),
    to: z.enum(LEGION_GO_PHASES).optional(),
    reason: z.string().optional(),
    decision: z.enum(["retry", "escalate"]).optional(),
    ...handoffSchemaFields(z),
  });
}

function requiredString(parameters: Record<string, unknown>, operation: string, name: string): string {
  const value = parameters[name];
  if (typeof value !== "string" || value.trim() === "") throw new Error(`${operation} requires ${name}`);
  return value;
}

function assertOperationInput(parameters: Record<string, unknown>, operation: string): void {
  const fields = OPERATION_FIELDS[operation];
  if (fields === undefined) throw new Error(`Unsupported Go Legion operation: ${operation}`);
  for (const name of Object.keys(parameters)) {
    if (name !== "op" && !fields.includes(name)) {
      throw new Error(`${operation} does not accept field "${name}"`);
    }
  }
}

async function grantFor(
  daemon: LegionGoDaemonClient,
  session: GoLegionToolSession
): Promise<string> {
  if (session.secret === undefined) {
    throw new Error("Go Legion session has no registered claim capability");
  }
  return (await daemon.grant({
    sessionId: session.sessionId,
    secret: session.secret,
    tree: session.tree,
    issue: session.issue,
  })).grantId;
}

function recordFrom(state: LegionGoState, issue: string): Readonly<Record<string, unknown>> {
  const record = state.issues[issue];
  if (record === undefined) throw new Error(`The Go daemon has no record for ${issue}`);
  return record;
}

/** The Go daemon's role-local workflow surface. The TypeScript-daemon tool remains separate until
 * Stage 7; no Go operation can schedule a worker. The handoff actions belong to every session but
 * the root architect: a phase worker, and a sub-architect (an architect whose issue is not its
 * tree). */
export function createGoLegionTool(deps: {
  readonly pi: PiApi;
  readonly daemon: () => LegionGoDaemonClient;
  readonly session: (context: SessionContext) => GoLegionToolSession;
  /** Told of each `handoff_complete` that succeeded: the session's phase is complete. */
  readonly onPhaseCompleted: (context: SessionContext) => void;
}): RegisteredTool {
  const { pi, daemon, session, onPhaseCompleted } = deps;
  return {
    name: "legion",
    label: "legion",
    description:
      "Perform the workflow operation the Go Legion daemon assigned this role. The daemon advances phases; this tool cannot spawn workers. " +
      HANDOFF_DESCRIPTION,
    defaultInactive: true,
    parameters: toolSchema(pi),
    execute: async (_id, parameters, signal, _onUpdate, context) => {
      try {
        const active = session(context);
        const operation = requiredString(parameters, "legion", "op");
        if (isHandoffOperation(operation)) {
          if (active.kind === "architect" && active.issue === active.tree) {
            throw new Error(`${operation} is not available to a root architect session`);
          }
          const result = await runHandoffAction({
            operation,
            parameters,
            signal,
            mintGrant: () => grantFor(daemon(), active),
          });
          if (operation === "handoff_complete" && result.isError !== true) onPhaseCompleted(context);
          return result;
        }
        if (!OPERATIONS[active.kind].includes(operation)) {
          throw new Error(`${operation} is not available to a ${active.kind} session`);
        }
        assertOperationInput(parameters, operation);
        const client = daemon();
        switch (operation) {
          case "read_record": {
            const issue = requiredString(parameters, operation, "issue");
            return jsonSuccess({ record: recordFrom(await client.state(), issue) });
          }
          case "register_gate": {
            const version = parameters.version;
            if (
              typeof version !== "number" ||
              !Number.isSafeInteger(version) ||
              version < 1
            ) {
              throw new Error("register_gate requires a positive integer version");
            }
            const grantId = await grantFor(client, active);
            await client.gateRegister({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
              artifactId: requiredString(parameters, operation, "artifactId"),
              version,
            });
            return jsonSuccess({});
          }
          case "release_children": {
            const issues = parameters.issues;
            if (!Array.isArray(issues) || !issues.every((issue) => typeof issue === "string")) {
              throw new Error("release_children requires issues");
            }
            const grantId = await grantFor(client, active);
            return jsonSuccess(await client.waveRelease({ grantId, issues }));
          }
          case "request_backward_move": {
            const grantId = await grantFor(client, active);
            await client.phaseBackward({
              grantId,
              to: requiredString(parameters, operation, "to") as (typeof LEGION_GO_PHASES)[number],
              reason: requiredString(parameters, operation, "reason"),
            });
            return jsonSuccess({});
          }
          case "retry_or_escalate": {
            const decision = parameters.decision;
            if (decision !== "retry" && decision !== "escalate") {
              throw new Error("retry_or_escalate requires decision retry or escalate");
            }
            const grantId = await grantFor(client, active);
            await client.phaseRetry({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
              decision,
            });
            return jsonSuccess({});
          }
          case "sign_off": {
            const grantId = await grantFor(client, active);
            await client.signOff({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
            });
            return jsonSuccess({});
          }
          default:
            throw new Error(`Unsupported Go Legion operation: ${operation}`);
        }
      } catch (error) {
        return toolFailure(error);
      }
    },
  };
}
