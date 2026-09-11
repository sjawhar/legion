import { LEGION_ROLES, type LegionRole } from "@legion/contracts";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "../pi-types";
import { toolFailure, toolSuccess } from "../tool-result";
import type { LegionDaemonClient } from "./daemon-client";

interface ArchitectSession {
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly secret: string;
}

const jsonSuccess = (details: Readonly<Record<string, unknown>>): ToolResult =>
  toolSuccess(JSON.stringify(details), details);

/** Legion's issue lifecycle, verbatim from Dispatch's `IssueStatuses`
 * (`packages/envoy/internal/dispatch/model/model.go`). Duplicated from `legion-state.ts`'s
 * `ISSUE_STATUSES`: the daemon and pi-envoy are independent packages with no shared runtime
 * dependency between them. */
const LIFECYCLE_STATUSES = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
] as const;

function isLifecycleStatus(value: string): value is (typeof LIFECYCLE_STATUSES)[number] {
  return (LIFECYCLE_STATUSES as readonly string[]).includes(value);
}

// pi.zod exposes only object/string/number/array/enum/unknown (no union or
// discriminatedUnion), so per-op typing cannot be expressed as a discriminated
// union at the schema layer. The schema stays a flat optional-fields bag; execute()
// below enforces, per op, which fields are actually accepted.
const LEGION_OP_FIELDS: Readonly<Record<string, readonly string[]>> = {
  set_status: ["issue", "status"],
  register_gate: ["issue", "askId"],
  release_wave: ["issues"],
  escalate: ["kind", "context"],
  spawn_worker: ["issue", "role", "task"],
};

function legionToolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  return z.object({
    op: z.enum(["set_status", "register_gate", "release_wave", "escalate", "spawn_worker"]),
    issue: z.string().optional(),
    status: z.enum(LIFECYCLE_STATUSES).optional(),
    askId: z.string().optional(),
    kind: z.enum(["re-file", "capacity", "cross-tree"]).optional(),
    context: z.unknown().optional(),
    issues: z.array(z.string()).optional(),
    rationale: z.string().optional(),
    role: z.enum(LEGION_ROLES).optional(),
    task: z.string().optional(),
  });
}

export function createLegionTool(deps: {
  readonly pi: PiApi;
  readonly roleDaemon: () => LegionDaemonClient;
  readonly architectSession: (context: SessionContext) => ArchitectSession;
}): RegisteredTool {
  const { pi, roleDaemon, architectSession } = deps;
  return {
    name: "legion",
    label: "legion",
    description:
      "Perform a Legion lifecycle write through the Legion daemon. " +
      'spawn_worker\'s response "status" means: "spawned" — a fresh pane just opened and is ' +
      'running now; "resumed" — an existing worker was prompted directly over its live socket, ' +
      "or (if its boot has not confirmed yet) its task was recorded to deliver once that boot " +
      'completes; "queued" — the running-worker cap is full, the task was recorded and this ' +
      'role will start on its own once a slot frees. Never re-spawn a role after "resumed" or ' +
      '"queued" — wait for the worker-started notification instead.',
    defaultInactive: true,
    parameters: legionToolSchema(pi),
    execute: async (_id, parameters, _signal, _onUpdate, context) => {
      try {
        const architect = architectSession(context);
        const daemon = roleDaemon();
        const sessionId = context.sessionManager.getSessionId();
        const stringInput = (name: string): string => {
          const value = parameters[name];
          if (typeof value !== "string")
            throw new Error(`${String(parameters.op)} requires ${name}`);
          return value;
        };
        const op = String(parameters.op);
        const allowedFields = LEGION_OP_FIELDS[op];
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
            if (typeof status !== "string" || !isLifecycleStatus(status)) {
              throw new Error("set_status requires a valid Legion issue status");
            }
            await daemon.issueStatus({
              tree: architect.tree,
              sessionId,
              secret: architect.secret,
              issue: stringInput("issue"),
              status,
            });
            return jsonSuccess({});
          }
          case "register_gate":
            await daemon.gatesRegister({
              tree: architect.tree,
              sessionId,
              secret: architect.secret,
              issue: stringInput("issue"),
              askId: stringInput("askId"),
            });
            return jsonSuccess({});
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
                tree: architect.tree,
                issues,
                sessionId,
                secret: architect.secret,
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
              tree: architect.tree,
              kind,
              context: parameters.context,
              sessionId,
              secret: architect.secret,
            });
            return jsonSuccess({});
          }
          case "spawn_worker": {
            const role = parameters.role;
            if (typeof role !== "string" || !LEGION_ROLES.includes(role as LegionRole)) {
              throw new Error("spawn_worker requires a valid Legion role");
            }
            return jsonSuccess(
              await daemon.spawnWorker({
                tree: architect.tree,
                sessionId,
                secret: architect.secret,
                issue: stringInput("issue"),
                role: role as LegionRole,
                task: stringInput("task"),
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
