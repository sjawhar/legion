import type { LegionRole } from "@legion/contracts";
import {
  LEGION_PHASE_BACKWARD_TARGETS,
  LegionGateRegisterRequest,
  type LegionGrant,
  type LegionIssue,
  type LegionState,
} from "@legion/contracts/legion-api";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "@legion/pi-shared/pi-types";
import { toolFailure, toolSuccess } from "@legion/pi-shared/tool-result";
import type { LegionDaemonClient } from "./daemon-client";

export type LegionToolRole = "architect" | "phase-worker" | "controller";

/** A root architect's, a sub-architect's or a phase worker's session: its claim, which mints its
 * grants on `/grants`' session form. `role` is the claim's Legion role (a sub-architect's is
 * `architect`); `kind` is what the tool's operation sets are keyed by. */
export interface LegionClaimToolSession {
  readonly kind: "architect" | "phase-worker";
  readonly role: LegionRole;
  readonly sessionId: string;
  readonly tree: string;
  readonly issue: string;
  readonly secret: string;
}

/** The controller's session: no tree, no issue; its grants are minted with its registration's
 * secret (`controllerGrant`, the `/grants` controller-session form). */
export interface LegionControllerToolSession {
  readonly kind: "controller";
  readonly sessionId: string;
}

export type LegionToolSession = LegionClaimToolSession | LegionControllerToolSession;

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
  "phase-worker": ["request_backward_move", "read_record", "handoff_complete", "resolve_threads"],
  controller: ["read_state", "set_status"],
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
  handoff_complete: ["summary", "verdict", "ready"],
  resolve_threads: ["repo", "threads"],
  read_state: [],
  set_status: ["issue", "status"],
};

const ISSUE_STATUSES = ["todo", "backlog", "icebox"] as const;

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
      "handoff_complete",
      "resolve_threads",
      "read_state",
      "set_status",
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
    to: z.enum(LEGION_PHASE_BACKWARD_TARGETS).optional(),
    reason: z.string().optional(),
    decision: z.enum(["retry", "escalate"]).optional(),
    summary: z.string().optional(),
    verdict: z.enum(["pass", "fail"]).optional(),
    ready: z.boolean().optional(),
    repo: z
      .string()
      .describe("resolve_threads: the pull request's repository, owner/name")
      .optional(),
    threads: z
      .array(z.string())
      .describe("resolve_threads: the GraphQL node id of each review thread to resolve")
      .optional(),
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

function recordFrom(state: LegionState, issue: string): LegionIssue {
  const record = state.issues[issue];
  if (record === undefined) throw new Error(`The daemon has no record for ${issue}`);
  return record;
}

/** The daemon's role-local workflow surface: no operation can schedule a worker. Every operation
 * that writes mints its own grant in-process and posts it with the request; nothing is written
 * to the pane. `handoff_complete` belongs to every session but the root architect's (a phase
 * worker, and a sub-architect: an architect whose issue is not its tree); `resolve_threads` to the
 * reviewer alone; `read_state` and `set_status` to the controller. */
export function createLegionTool(deps: {
  readonly pi: PiApi;
  readonly daemon: () => LegionDaemonClient;
  readonly session: (context: SessionContext) => LegionToolSession;
  /** Mints the controller's grant, authenticated by its registration's secret. */
  readonly controllerGrant: (sessionId: string) => Promise<LegionGrant>;
  /** Told of each `handoff_complete` that succeeded: the session's phase is complete. */
  readonly onPhaseCompleted: (context: SessionContext) => void;
  /** The id of the document `issue` carries under `reference` (`spec`, a slug, or a filename),
   * looked up in Dispatch as the Dispatch tools do; throws naming the reference when none matches,
   * or when it names two documents. */
  readonly resolveDocument: (issue: string, reference: string) => Promise<string>;
}): RegisteredTool {
  const { pi, daemon, session, controllerGrant, onPhaseCompleted, resolveDocument } = deps;
  const grantFor = async (
    client: LegionDaemonClient,
    active: LegionClaimToolSession
  ): Promise<string> =>
    (
      await client.grant({
        sessionId: active.sessionId,
        secret: active.secret,
        tree: active.tree,
        issue: active.issue,
      })
    ).grantId;
  return {
    name: "legion",
    label: "legion",
    description:
      "Perform the workflow operation the Legion daemon assigned this role. The daemon advances phases; this tool cannot spawn workers. " +
      "handoff_complete (phase workers and sub-architects; the daemon accepts a completion only from the role working the issue's current phase) " +
      "reports this phase complete: `summary` (two sentences for the architect, or the merger's READY packet), `verdict` pass|fail when your role's " +
      "instructions require one, `ready: true` for the merger's READY. A handoff is a committed file, `.legion/<issue>/<phase>.json`, that the daemon " +
      "reads at the head of the issue branch on GitHub when you complete, so commit it and push the branch (`jj git push`) before calling; no CLI " +
      "command pushes or completes a handoff for you. A completion is refused HANDOFF_BRANCH_MISSING, HANDOFF_AUTHOR_MISMATCH, HANDOFF_FILE_MISSING, " +
      "HANDOFF_INVALID (the fields it names), HANDOFF_NOT_NEW (nothing pushed since your last completion), READY_HEAD_CARRIES_HANDOFFS or " +
      "READY_CHECKS_NOT_GREEN; fix what it names, push, and complete again, since a refused completion changed nothing. " +
      "resolve_threads (the reviewer alone) has the daemon resolve, as the pull request author's App, the review threads you name by GraphQL node id " +
      "(`threads`, one or more you have replied on) on your issue's recorded pull request in `repo` (owner/name); the implementer resolves its own " +
      "threads with gh. The controller's read_state returns the daemon's whole state and set_status moves an issue to todo, backlog or icebox. " +
      "What a later phase needs goes in your handoff; a question for another live role goes to its role topic with envoy_publish.",
    defaultInactive: true,
    parameters: toolSchema(pi),
    execute: async (_id, parameters, _signal, _onUpdate, context) => {
      try {
        const active = session(context);
        const operation = requiredString(parameters, "legion", "op");
        if (!OPERATIONS[active.kind].includes(operation)) {
          throw new Error(`${operation} is not available to a ${active.kind} session`);
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
          const { grantId } = await controllerGrant(active.sessionId);
          await client.issueStatus({ grantId, issue, status });
          return jsonSuccess({});
        }
        switch (operation) {
          case "read_record": {
            const issue = requiredString(parameters, operation, "issue");
            return jsonSuccess({ record: recordFrom(await client.state(), issue) });
          }
          case "handoff_complete": {
            if (active.kind === "architect" && active.issue === active.tree) {
              throw new Error(
                "handoff_complete is not available to a root architect session, which runs no phase"
              );
            }
            const summary = requiredString(parameters, operation, "summary");
            const { verdict, ready } = parameters;
            if (verdict !== undefined && verdict !== "pass" && verdict !== "fail") {
              throw new Error("handoff_complete's verdict is pass or fail");
            }
            if (ready !== undefined && typeof ready !== "boolean") {
              throw new Error("handoff_complete's ready is true or false");
            }
            const grantId = await grantFor(client, active);
            const answer = await client.handoffComplete({
              grantId,
              summary,
              verdict: verdict ?? "",
              ready: ready ?? false,
            });
            onPhaseCompleted(context);
            return jsonSuccess(answer);
          }
          case "resolve_threads": {
            if (active.kind !== "phase-worker" || active.role !== "reviewer") {
              throw new Error(
                "resolve_threads is available to the reviewer alone: the daemon resolves review threads as the pull request author's App only for the reviewer's grant; the pull request's author resolves its own threads with gh"
              );
            }
            const threads = parameters.threads;
            if (
              !Array.isArray(threads) ||
              threads.length === 0 ||
              !threads.every(
                (thread): thread is string => typeof thread === "string" && thread.trim() !== ""
              )
            ) {
              throw new Error(
                "resolve_threads requires threads: the GraphQL node id of each review thread to resolve, at least one, none empty"
              );
            }
            const repo = requiredString(parameters, operation, "repo");
            const { pullRequest } = recordFrom(await client.state(), active.issue);
            if (pullRequest === undefined) {
              throw new Error(
                `${active.issue} has no pull request recorded, so there is no pull request whose threads to resolve; the daemon records it once the implementer opens one`
              );
            }
            const grantId = await grantFor(client, active);
            return jsonSuccess(
              await client.threadsResolve({ grantId, repo, number: pullRequest.number, threads })
            );
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
              ) as (typeof LEGION_PHASE_BACKWARD_TARGETS)[number],
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
