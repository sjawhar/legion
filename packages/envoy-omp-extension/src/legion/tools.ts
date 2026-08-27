import { ARCHITECT_MUTABLE_LABELS, type LegionRole } from "@legion/contracts";
import type { LegionDaemonClient } from "./daemon-client";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "./pi-types";

type ArchitectSession = {
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly secret: string;
};

export function toolSuccess(details: Readonly<Record<string, unknown>>): ToolResult {
  return { content: [{ type: "text", text: JSON.stringify(details) }], details };
}

export function toolFailure(error: unknown): ToolResult {
  const message = error instanceof Error ? error.message : String(error);
  return { content: [{ type: "text", text: message }], details: {}, isError: true };
}

// pi.zod exposes only object/string/number/array/enum/unknown (no union or
// discriminatedUnion), so per-op typing cannot be expressed as a discriminated
// union at the schema layer. The schema stays a flat optional-fields bag; execute()
// below enforces, per op, which fields are actually accepted.
const LEGION_OP_FIELDS: Readonly<Record<string, readonly string[]>> = {
  issue_create: ["title", "body", "labels"],
  wave_release: ["children"],
  comment: ["issue", "body"],
  post_spec: ["issue", "body"],
  label_add: ["issue", "label"],
  escalate: ["kind", "context"],
  request_refile: ["issue", "rationale"],
  issue_close: ["issue", "comment"],
  merge_gate: ["pr"],
};

export function legionToolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  return z.object({
    op: z.enum([
      "issue_create",
      "wave_release",
      "comment",
      "post_spec",
      "label_add",
      "escalate",
      "request_refile",
      "issue_close",
      "merge_gate",
    ]),
    title: z.string().optional(),
    body: z.string().optional(),
    labels: z.array(z.enum(ARCHITECT_MUTABLE_LABELS)).optional(),
    children: z.array(z.string()).optional(),
    issue: z.string().optional(),
    label: z.enum(ARCHITECT_MUTABLE_LABELS).optional(),
    kind: z.enum(["re-file", "capacity", "cross-tree"]).optional(),
    context: z.unknown().optional(),
    rationale: z.string().optional(),
    comment: z.string().optional(),
    pr: z.number().optional(),
  });
}

export function envoyDispatchToolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  return z.object({
    parent: z.string(),
    subject: z.string(),
    body: z.string(),
    ask: z.array(z.unknown()).optional(),
    urgency: z.enum(["low", "med", "high", "blocking"]).optional(),
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
    description: "Perform a Legion lifecycle write through the Legion daemon.",
    defaultInactive: true,
    parameters: legionToolSchema(pi),
    execute: async (_id, parameters, _signal, _onUpdate, context) => {
      try {
        const architect = architectSession(context);
        const daemon = roleDaemon();
        const stringInput = (name: string): string => {
          const value = parameters[name];
          if (typeof value !== "string")
            throw new Error(`${String(parameters.op)} requires ${name}`);
          return value;
        };
        const numberInput = (name: string): number => {
          const value = parameters[name];
          if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) {
            throw new Error(`${String(parameters.op)} requires a positive integer ${name}`);
          }
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
          case "merge_gate":
            return toolSuccess(
              await daemon.mergeGate({
                tree: architect.tree,
                pr: numberInput("pr"),
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
              })
            );
          case "issue_create": {
            const labels = parameters.labels;
            if (
              labels !== undefined &&
              (!Array.isArray(labels) ||
                labels.some(
                  (label) =>
                    typeof label !== "string" ||
                    !ARCHITECT_MUTABLE_LABELS.includes(
                      label as (typeof ARCHITECT_MUTABLE_LABELS)[number]
                    )
                ))
            ) {
              throw new Error("issue_create labels must use architect-mutable Legion labels");
            }
            return toolSuccess(
              await daemon.issueCreate({
                tree: architect.tree,
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
                title: stringInput("title"),
                body: stringInput("body"),
                labels: labels ?? [],
              })
            );
          }
          case "wave_release": {
            const children = parameters.children;
            if (!Array.isArray(children) || children.some((child) => typeof child !== "string")) {
              throw new Error("wave_release requires children");
            }
            return toolSuccess(
              await daemon.waveRelease({
                tree: architect.tree,
                children,
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
              })
            );
          }
          case "comment":
            return toolSuccess(
              await daemon.comment({
                tree: architect.tree,
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
                issue: stringInput("issue"),
                body: stringInput("body"),
              })
            );
          case "post_spec":
            await daemon.postBody({
              tree: architect.tree,
              sessionId: context.sessionManager.getSessionId(),
              secret: architect.secret,
              issue: stringInput("issue"),
              body: stringInput("body"),
            });
            return toolSuccess({});
          case "label_add": {
            const label = stringInput("label");
            if (
              !ARCHITECT_MUTABLE_LABELS.includes(label as (typeof ARCHITECT_MUTABLE_LABELS)[number])
            ) {
              throw new Error("label changes must use architect-mutable Legion labels");
            }
            const architectLabel = label as (typeof ARCHITECT_MUTABLE_LABELS)[number];
            return toolSuccess(
              await daemon.labels({
                tree: architect.tree,
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
                issue: stringInput("issue"),
                add: [architectLabel],
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
              sessionId: context.sessionManager.getSessionId(),
              secret: architect.secret,
            });
            return toolSuccess({});
          }
          case "request_refile":
            await daemon.escalate({
              tree: architect.tree,
              sessionId: context.sessionManager.getSessionId(),
              secret: architect.secret,
              kind: "re-file",
              context: { issue: stringInput("issue"), rationale: stringInput("rationale") },
            });
            return toolSuccess({});
          case "issue_close": {
            const comment = parameters.comment;
            if (comment !== undefined && typeof comment !== "string")
              throw new Error("issue_close comment must be a string");
            await daemon.issueClose({
              tree: architect.tree,
              sessionId: context.sessionManager.getSessionId(),
              secret: architect.secret,
              issue: stringInput("issue"),
              ...(comment === undefined ? {} : { comment }),
            });
            return toolSuccess({});
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

export function createEnvoyDispatchTool(deps: {
  readonly pi: PiApi;
  readonly roleDaemon: () => LegionDaemonClient;
  readonly architectSession: (context: SessionContext) => ArchitectSession;
}): RegisteredTool {
  const { pi, roleDaemon, architectSession } = deps;
  return {
    name: "envoy_dispatch",
    label: "envoy_dispatch",
    description:
      "Open an architect-owned Dispatch thread and route replies back to this Legion role.",
    defaultInactive: true,
    parameters: envoyDispatchToolSchema(pi),
    execute: async (_id, parameters, _signal, _onUpdate, context) => {
      try {
        const architect = architectSession(context);
        const { parent, subject, body, ask, urgency } = parameters;
        if (typeof parent !== "string" || typeof subject !== "string" || typeof body !== "string") {
          throw new Error("envoy_dispatch requires parent, subject, and body");
        }
        let dispatchUrgency: "low" | "med" | "high" | "blocking" | undefined;
        if (urgency === undefined) {
          dispatchUrgency = undefined;
        } else if (
          urgency === "low" ||
          urgency === "med" ||
          urgency === "high" ||
          urgency === "blocking"
        ) {
          dispatchUrgency = urgency;
        } else {
          throw new Error("envoy_dispatch urgency must be low, med, high, or blocking");
        }
        const result = await roleDaemon().dispatchThread({
          tree: architect.tree,
          issue: architect.issue,
          role: architect.role,
          sessionId: context.sessionManager.getSessionId(),
          secret: architect.secret,
          parent,
          subject,
          body,
          ...(ask === undefined ? {} : { ask }),
          ...(dispatchUrgency === undefined ? {} : { urgency: dispatchUrgency }),
        });
        return toolSuccess(result);
      } catch (error) {
        return toolFailure(error);
      }
    },
  };
}
