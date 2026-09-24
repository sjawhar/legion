import { randomUUID } from "node:crypto";
import {
  ISSUE_STATUSES,
  isIssueStatus,
  LEGION_ROLES,
  LegionDaemonApi,
  type LegionRole,
} from "@legion/contracts";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "../pi-types";
import { toolFailure, toolSuccess } from "../tool-result";
import type { LegionDaemonClient } from "./daemon-client";
import {
  HANDOFF_DESCRIPTION,
  HANDOFF_OPERATIONS,
  handoffSchemaFields,
  isHandoffOperation,
  runHandoffAction,
} from "./handoff-actions";

/** The session's registered Legion role. A root architect runs the architect operations; a phase
 * worker the handoff actions, and a sub-architect (a phase worker whose role is architect) both. */
interface LegionToolSession {
  readonly kind: "root-architect" | "phase-worker";
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly secret: string;
}

const jsonSuccess = (details: Readonly<Record<string, unknown>>): ToolResult =>
  toolSuccess(JSON.stringify(details), details);

/** A Dispatch artifact id: what `artifact.approved` and its siblings carry as `artifact_id`, and
 * therefore the only value `register_gate` may record. The daemon's contract owns the
 * definition (`LegionDaemonApi.GatesRegister.request`'s `artifactId`, a UUID); this check exists
 * to fail with a message that says where the id comes from, before the round trip. */
function isDispatchArtifactId(value: string): boolean {
  return LegionDaemonApi.GatesRegister.request.shape.artifactId.safeParse(value).success;
}

// pi.zod exposes only object/string/number/boolean/array/enum/unknown (no union or
// discriminatedUnion), so per-op typing cannot be expressed as a discriminated
// union at the schema layer. The schema stays a flat optional-fields bag; execute()
// below enforces, per op, which fields are actually accepted.
const ARCHITECT_OP_FIELDS: Readonly<Record<string, readonly string[]>> = {
  set_status: ["issue", "status"],
  register_gate: ["issue", "artifactId", "version"],
  release_wave: ["issues"],
  escalate: ["kind", "context"],
  spawn_worker: ["issue", "role", "task"],
};

function legionToolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  return z.object({
    op: z.enum([...Object.keys(ARCHITECT_OP_FIELDS), ...HANDOFF_OPERATIONS]),
    issue: z.string().optional(),
    status: z.enum(ISSUE_STATUSES).optional(),
    artifactId: z.string().optional(),
    version: z.number().optional(),
    kind: z.enum(["re-file", "capacity", "cross-tree"]).optional(),
    context: z.unknown().optional(),
    issues: z.array(z.string()).optional(),
    rationale: z.string().optional(),
    role: z.enum(LEGION_ROLES).optional(),
    task: z.string().optional(),
    ...handoffSchemaFields(z),
  });
}

export function createLegionTool(deps: {
  readonly pi: PiApi;
  readonly roleDaemon: () => LegionDaemonClient;
  readonly session: (context: SessionContext) => LegionToolSession;
  /** Told of each `handoff_complete` that succeeded: the session's phase is complete. */
  readonly onPhaseCompleted: (context: SessionContext) => void;
}): RegisteredTool {
  const { pi, roleDaemon, session, onPhaseCompleted } = deps;
  return {
    name: "legion",
    label: "legion",
    description:
      "Perform a Legion lifecycle write through the Legion daemon. " +
      "Architect operations (set_status, register_gate, release_wave, escalate, spawn_worker): " +
      "register_gate records the root spec document a human must approve: `artifactId` is the " +
      "document id (a UUID) and `version` the version number, both copied from the `artifact` and " +
      "`version` fields of dispatch_request_approval's result — never the slug or file name you " +
      "passed to that tool. " +
      'spawn_worker\'s response "status" means: "spawned" — a fresh pane just opened and is ' +
      'running now; "resumed" — an existing worker was prompted directly over its live socket ' +
      "and its turn started, or (if its boot has not confirmed yet) its task was recorded to " +
      'deliver once that boot completes; "queued" — the task was recorded and this role will ' +
      "start on its own: either the running-worker cap is full, or the live worker acknowledged " +
      "the task without starting a turn and the daemon is retrying it. Never re-spawn a role " +
      'after "resumed" or "queued" — wait for the worker-started notification instead. A call ' +
      'that fails with "got no response in 3 attempts" was retried by the plugin with one ' +
      "request id; the daemon may still have received it — read legion state " +
      "(workerAdmission.queue, and the role in roles) before sending it again. A spawn_worker " +
      "identical to the task already queued for the role changes nothing and is not announced again. " +
      HANDOFF_DESCRIPTION,
    defaultInactive: true,
    parameters: legionToolSchema(pi),
    execute: async (_id, parameters, signal, _onUpdate, context) => {
      try {
        const active = session(context);
        const sessionId = context.sessionManager.getSessionId();
        const op = String(parameters.op);
        if (isHandoffOperation(op)) {
          if (active.kind !== "phase-worker") {
            throw new Error(`${op} is not available to a root architect session`);
          }
          const result = await runHandoffAction({
            operation: op,
            parameters,
            signal,
            mintGrant: async () =>
              (
                await roleDaemon().grant({
                  tree: active.tree,
                  issue: active.issue,
                  sessionId,
                  secret: active.secret,
                })
              ).grantId,
          });
          if (op === "handoff_complete" && result.isError !== true) onPhaseCompleted(context);
          return result;
        }
        if (active.role !== "architect") {
          throw new Error(`${op} is not available to a ${active.role} session`);
        }
        const daemon = roleDaemon();
        const stringInput = (name: string): string => {
          const value = parameters[name];
          if (typeof value !== "string")
            throw new Error(`${String(parameters.op)} requires ${name}`);
          return value;
        };
        const allowedFields = ARCHITECT_OP_FIELDS[op];
        if (allowedFields) {
          for (const key of Object.keys(parameters)) {
            if (key !== "op" && !allowedFields.includes(key)) {
              throw new Error(`${op} does not accept field "${key}"`);
            }
          }
        }
        switch (parameters.op) {
          case "set_status": {
            const status = parameters.status;
            if (typeof status !== "string" || !isIssueStatus(status)) {
              throw new Error("set_status requires a valid Legion issue status");
            }
            await daemon.issueStatus({
              tree: active.tree,
              sessionId,
              secret: active.secret,
              issue: stringInput("issue"),
              status,
            });
            return jsonSuccess({});
          }
          case "register_gate": {
            const version = parameters.version;
            if (typeof version !== "number" || !Number.isSafeInteger(version) || version <= 0) {
              throw new Error("register_gate requires a positive integer version");
            }
            const artifactId = stringInput("artifactId");
            if (!isDispatchArtifactId(artifactId)) {
              throw new Error(
                `register_gate requires artifactId to be the document id (a UUID) from dispatch_request_approval's result, not "${artifactId}"`
              );
            }
            await daemon.gatesRegister({
              tree: active.tree,
              sessionId,
              secret: active.secret,
              issue: stringInput("issue"),
              artifactId,
              version,
            });
            return jsonSuccess({});
          }
          case "release_wave": {
            const issues = parameters.issues;
            if (
              !Array.isArray(issues) ||
              !issues.every((issue: unknown): issue is string => typeof issue === "string")
            ) {
              throw new Error("release_wave requires issues");
            }
            return jsonSuccess(
              await daemon.releaseWave({
                tree: active.tree,
                issues,
                sessionId,
                secret: active.secret,
              })
            );
          }
          case "escalate": {
            const kind = stringInput("kind");
            if (kind !== "re-file" && kind !== "capacity" && kind !== "cross-tree") {
              throw new Error("Unknown Legion escalation kind");
            }
            if (!("context" in parameters) || parameters.context === undefined)
              throw new Error("escalate requires context");
            await daemon.escalate({
              tree: active.tree,
              kind,
              context: parameters.context,
              sessionId,
              secret: active.secret,
            });
            return jsonSuccess({});
          }
          case "spawn_worker": {
            const role = parameters.role;
            if (typeof role !== "string" || !LEGION_ROLES.includes(role as LegionRole)) {
              throw new Error("spawn_worker requires a valid Legion role");
            }
            // One request id per tool call: the daemon dedupes repeats of it, so a transport
            // retry (daemon-client.ts) can never queue the same task twice.
            const requestId = randomUUID();
            return jsonSuccess(
              await daemon.spawnWorker({
                tree: active.tree,
                sessionId,
                secret: active.secret,
                issue: stringInput("issue"),
                role: role as LegionRole,
                task: stringInput("task"),
                requestId,
              })
            );
          }
          default:
            throw new Error(`Unsupported legion operation: ${String(parameters.op)}`);
        }
      } catch (error) {
        return toolFailure(error);
      }
    },
  };
}
