import {
  LEGION_WORKFLOW_PHASES,
  LegionGateRegisterRequest,
  type LegionState,
} from "@legion/contracts/legion-api";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "../pi-types";
import { toolFailure, toolSuccess } from "../tool-result";
import type { LegionDaemonClient } from "./daemon-client";
import {
  HANDOFF_DESCRIPTION,
  HANDOFF_OPERATIONS,
  handoffSchemaFields,
  isHandoffOperation,
  rootArchitectHandoffRefusal,
  runHandoffAction,
} from "./handoff-actions";

export type LegionToolRole = "architect" | "phase-worker";

export interface LegionToolSession {
  readonly kind: LegionToolRole;
  readonly sessionId: string;
  readonly tree: string;
  readonly issue: string;
  readonly secret: string;
}

const OPERATIONS: Readonly<Record<LegionToolRole, readonly string[]>> = {
  architect: [
    "register_gate",
    "release_children",
    "request_backward_move",
    "retry_or_escalate",
    "sign_off",
    "close_root",
    "park_child",
    "rerun_child",
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
  close_root: ["issue", "reason"],
  park_child: ["issue"],
  rerun_child: ["issue"],
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
      "close_root",
      "park_child",
      "rerun_child",
      "read_record",
      ...HANDOFF_OPERATIONS,
    ]),
    issue: z.string().optional(),
    artifactId: z
      .string()
      .describe(
        "register_gate's root spec document: its artifact id, slug, or filename, as the Dispatch tools take it"
      )
      .optional(),
    version: z.number().optional(),
    issues: z.array(z.string()).optional(),
    to: z.enum(LEGION_WORKFLOW_PHASES).optional(),
    reason: z.string().optional(),
    decision: z.enum(["retry", "escalate"]).optional(),
    ...handoffSchemaFields(z),
  });
}

function requiredString(
  parameters: Record<string, unknown>,
  operation: string,
  name: string
): string {
  const value = parameters[name];
  if (typeof value !== "string" || value.trim() === "")
    throw new Error(`${operation} requires ${name}`);
  return value;
}

function assertOperationInput(parameters: Record<string, unknown>, operation: string): void {
  const fields = OPERATION_FIELDS[operation];
  if (fields === undefined) throw new Error(`Unsupported legion operation: ${operation}`);
  for (const name of Object.keys(parameters)) {
    if (name !== "op" && !fields.includes(name)) {
      throw new Error(`${operation} does not accept field "${name}"`);
    }
  }
}

async function grantFor(daemon: LegionDaemonClient, session: LegionToolSession): Promise<string> {
  return (
    await daemon.grant({
      sessionId: session.sessionId,
      secret: session.secret,
      tree: session.tree,
      issue: session.issue,
    })
  ).grantId;
}

function recordFrom(state: LegionState, issue: string): Readonly<Record<string, unknown>> {
  const record = state.issues[issue];
  if (record === undefined) throw new Error(`The daemon has no record for ${issue}`);
  return record;
}

/** The daemon's role-local workflow surface: no operation can schedule a worker. The handoff
 * actions belong to every session but the root architect: a phase worker, and a sub-architect (an
 * architect whose issue is not its tree). */
export function createLegionTool(deps: {
  readonly pi: PiApi;
  readonly daemon: () => LegionDaemonClient;
  readonly session: (context: SessionContext) => LegionToolSession;
  /** Told of each `handoff_complete` that succeeded: the session's phase is complete. */
  readonly onPhaseCompleted: (context: SessionContext) => void;
  /** The id of the document `issue` carries under `reference` (`spec`, a slug, or a filename),
   * looked up in Dispatch as the Dispatch tools do; throws naming the reference when none matches,
   * or when it names two documents. */
  readonly resolveDocument: (issue: string, reference: string) => Promise<string>;
}): RegisteredTool {
  const { pi, daemon, session, onPhaseCompleted, resolveDocument } = deps;
  return {
    name: "legion",
    label: "legion",
    description:
      "Perform the workflow operation the Legion daemon assigned this role. The daemon advances phases; this tool cannot spawn workers. " +
      HANDOFF_DESCRIPTION,
    defaultInactive: true,
    parameters: toolSchema(pi),
    execute: async (_id, parameters, signal, _onUpdate, context) => {
      try {
        const active = session(context);
        const operation = requiredString(parameters, "legion", "op");
        if (isHandoffOperation(operation)) {
          if (active.kind === "architect" && active.issue === active.tree) {
            throw new Error(rootArchitectHandoffRefusal(operation));
          }
          return await runHandoffAction({
            operation,
            parameters,
            signal,
            mintGrant: () => grantFor(daemon(), active),
            onPhaseCompleted: () => onPhaseCompleted(context),
          });
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
            if (typeof version !== "number" || !Number.isSafeInteger(version) || version < 1) {
              throw new Error("register_gate requires a positive integer version");
            }
            // The gate is the tree root's, registered by the root's own architect, as the daemon
            // requires: anyone else is refused before a lookup could answer for the wrong issue.
            const issue = requiredString(parameters, operation, "issue");
            if (active.issue !== active.tree) {
              throw new Error(
                `the design gate belongs to the tree root ${active.tree}; its root architect registers it`
              );
            }
            if (issue !== active.issue) {
              throw new Error(
                `the design gate belongs to the tree root ${active.tree}; register it there`
              );
            }
            // The daemon takes the document's id alone; `spec`, a slug or a filename, the
            // references the Dispatch tools accept, is looked up first, so the architect's first
            // call names the document however it knows it.
            const reference = requiredString(parameters, operation, "artifactId");
            const isId = LegionGateRegisterRequest.shape.artifactId.safeParse(reference).success;
            const artifactId = isId ? reference : await resolveDocument(issue, reference);
            const grantId = await grantFor(client, active);
            await client.gateRegister({ grantId, issue, artifactId, version });
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
              to: requiredString(
                parameters,
                operation,
                "to"
              ) as (typeof LEGION_WORKFLOW_PHASES)[number],
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
          case "close_root": {
            const grantId = await grantFor(client, active);
            await client.rootClose({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
              reason: requiredString(parameters, operation, "reason"),
            });
            return jsonSuccess({});
          }
          case "park_child":
          case "rerun_child": {
            const grantId = await grantFor(client, active);
            const request = { grantId, issue: requiredString(parameters, operation, "issue") };
            await (operation === "park_child"
              ? client.childPark(request)
              : client.childRerun(request));
            return jsonSuccess({});
          }
          default:
            throw new Error(`Unsupported legion operation: ${operation}`);
        }
      } catch (error) {
        return toolFailure(error);
      }
    },
  };
}
