import type { LegionRole } from "@legion/contracts";
import {
  LegionGateRegisterRequest,
  type LegionGrant,
  type LegionPhase,
} from "@legion/contracts/legion-api";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import {
  type ClaimCapability,
  executePhaseOp,
  isPhaseOp,
  legionToolDescription,
  mintClaimGrant,
  phaseOpParameters,
} from "@legion/pi-shared/legion-phase-ops";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "@legion/pi-shared/pi-types";
import { toolFailure, toolSuccess } from "@legion/pi-shared/tool-result";

export type LegionToolRole = "architect" | "phase-worker" | "controller";

/** A root architect's, a sub-architect's or a phase worker's session: its claim, which mints its
 * grants on `/grants`' session form. `role` is the claim's Legion role (a sub-architect's is
 * `architect`); `kind` is what the tool's operation sets are keyed by. */
export interface LegionClaimToolSession extends ClaimCapability {
  readonly kind: "architect" | "phase-worker";
}

/** The controller's session: no tree, no issue; its grants are minted with its registration's
 * secret (`controllerGrant`, the `/grants` controller-session form). */
export interface LegionControllerToolSession {
  readonly kind: "controller";
  readonly sessionID: string;
}

export type LegionToolSession = LegionClaimToolSession | LegionControllerToolSession;

/** Each session kind's operations, in the order the tool's `op` enum lists them. The phase ops
 * (`@legion/pi-shared/legion-phase-ops`) are an architect's too. */
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
    "handoff_complete",
  ],
  "phase-worker": ["request_backward_move", "read_record", "handoff_complete"],
  controller: ["read_state", "set_status"],
};

/** The fields each operation beyond the phase ops takes; a phase op's are `PhaseOpArguments`'. */
const OPERATION_FIELDS: Readonly<Record<string, readonly string[]>> = {
  register_gate: ["issue", "artifactId", "version"],
  release_children: ["issues"],
  retry_or_escalate: ["issue", "decision"],
  sign_off: ["issue"],
  close_root: ["issue", "reason"],
  park_child: ["issue"],
  rerun_child: ["issue"],
  read_state: [],
  set_status: ["issue", "status"],
};

const ISSUE_STATUSES = ["todo", "backlog", "icebox"] as const;

const jsonSuccess = (details: Readonly<Record<string, unknown>>): ToolResult =>
  toolSuccess(JSON.stringify(details), details);

/** The tool's one object schema, `op` choosing the operation; its fields and the `op` values are
 * in the order `tools.test.ts` pins. */
function toolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  const phase = phaseOpParameters(z);
  return z.object({
    op: z.enum([...OPERATIONS.architect, ...OPERATIONS.controller]),
    issue: phase.issue,
    artifactId: z
      .string()
      .describe(
        "register_gate's root spec document: its artifact id, slug, or filename, as a dispatch command's --artifact takes it"
      )
      .optional(),
    version: z.number().optional(),
    issues: z.array(z.string()).optional(),
    to: phase.to,
    reason: phase.reason,
    decision: z.enum(["retry", "escalate"]).optional(),
    summary: phase.summary,
    verdict: phase.verdict,
    ready: phase.ready,
    status: z.enum(ISSUE_STATUSES).optional(),
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

/** The daemon's role-local workflow surface: no operation can schedule a worker. Every operation
 * that writes mints its own grant in-process and posts it with the request; nothing is written
 * to the pane. The phase ops run on the executor every harness shares (`executePhaseOp`):
 * `handoff_complete` belongs to every session but the root architect's (a phase worker, and a
 * sub-architect: an architect whose issue is not its tree), and finds the commit it reports in the
 * pane first (`handoffCommit`); `read_state` and `set_status` belong to the controller. */
export function createLegionTool(deps: {
  readonly pi: PiApi;
  readonly daemon: () => LegionDaemonClient;
  readonly session: (context: SessionContext) => LegionToolSession;
  /** Mints the controller's grant, authenticated by its registration's secret. */
  readonly controllerGrant: (sessionId: string) => Promise<LegionGrant>;
  /** Told of each `handoff_complete` that succeeded: the session's phase is complete. */
  readonly onPhaseCompleted: (context: SessionContext) => void;
  /** The commit a completion of `phase` by `role` reports: for a file-backed phase the role
   * works, the pushed commit carrying `.legion/<issue>/<phase>.json`, found with the pane's jj
   * (`findHandoffCommit`); otherwise the commit the workspace stands on. Throws naming the remedy
   * when the handoff is uncommitted, missing, not on this branch, another pane's, or unpushed. */
  readonly handoffCommit: (phase: LegionPhase, role: LegionRole) => Promise<string>;
  /** The id of the document `issue` carries under `reference` (`spec`, a slug, or a filename),
   * looked up in Dispatch as a `dispatch` command's `--artifact` is; throws naming the reference
   * when none matches, or when it names two documents. */
  readonly resolveDocument: (issue: string, reference: string) => Promise<string>;
}): RegisteredTool {
  const { pi, daemon, session, controllerGrant, onPhaseCompleted, handoffCommit, resolveDocument } =
    deps;
  return {
    name: "legion",
    label: "legion",
    // No sentence on read_record or request_backward_move, and the controller's operations after
    // handoff_complete: the exact text tools.test.ts pins, so a change to the shared template
    // cannot reach an Oh My Pi agent unnoticed.
    description: legionToolDescription({
      envoyPublish: "envoy_publish",
      after:
        "The controller's read_state returns the daemon's whole state and set_status moves an issue to todo, backlog or icebox. ",
    }),
    defaultInactive: true,
    parameters: toolSchema(pi),
    execute: async (_id, parameters, _signal, _onUpdate, context) => {
      try {
        const active = session(context);
        const operation = requiredString(parameters, "legion", "op");
        if (!OPERATIONS[active.kind].includes(operation)) {
          throw new Error(`${operation} is not available to a ${active.kind} session`);
        }
        if (active.kind !== "controller" && isPhaseOp(operation)) {
          const answer = await executePhaseOp(
            { daemon: daemon(), claim: active, handoffCommit },
            parameters
          );
          if (operation === "handoff_complete") onPhaseCompleted(context);
          return jsonSuccess(answer);
        }
        assertOperationInput(parameters, operation);
        const client = daemon();
        if (active.kind === "controller") {
          if (operation === "read_state") return jsonSuccess(await client.state());
          // set_status, the controller's one write: validated whole before its grant is minted.
          const issue = requiredString(parameters, operation, "issue");
          const status = ISSUE_STATUSES.find((candidate) => candidate === parameters.status);
          if (status === undefined) {
            throw new Error(`set_status requires status ${ISSUE_STATUSES.join(", ")}`);
          }
          const { grantId } = await controllerGrant(active.sessionID);
          await client.issueStatus({ grantId, issue, status });
          return jsonSuccess({});
        }
        switch (operation) {
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
            // references a `dispatch` command's --artifact accepts, is looked up first, so the
            // architect's first call names the document however it knows it.
            const reference = requiredString(parameters, operation, "artifactId");
            const isId = LegionGateRegisterRequest.shape.artifactId.safeParse(reference).success;
            const artifactId = isId ? reference : await resolveDocument(issue, reference);
            const grantId = await mintClaimGrant(client, active);
            await client.gateRegister({ grantId, issue, artifactId, version });
            return jsonSuccess({});
          }
          case "release_children": {
            const issues = parameters.issues;
            if (!Array.isArray(issues) || !issues.every((issue) => typeof issue === "string")) {
              throw new Error("release_children requires issues");
            }
            const grantId = await mintClaimGrant(client, active);
            return jsonSuccess(await client.waveRelease({ grantId, issues }));
          }
          case "retry_or_escalate": {
            const decision = parameters.decision;
            if (decision !== "retry" && decision !== "escalate") {
              throw new Error("retry_or_escalate requires decision retry or escalate");
            }
            const grantId = await mintClaimGrant(client, active);
            await client.phaseRetry({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
              decision,
            });
            return jsonSuccess({});
          }
          case "sign_off": {
            const grantId = await mintClaimGrant(client, active);
            await client.signOff({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
            });
            return jsonSuccess({});
          }
          case "close_root": {
            const grantId = await mintClaimGrant(client, active);
            await client.rootClose({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
              reason: requiredString(parameters, operation, "reason"),
            });
            return jsonSuccess({});
          }
          case "park_child":
          case "rerun_child": {
            const grantId = await mintClaimGrant(client, active);
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
