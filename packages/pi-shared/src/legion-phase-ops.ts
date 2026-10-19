import { type LegionRole, zodSchemaApi } from "@legion/contracts";
import {
  LEGION_PHASE_BACKWARD_TARGETS,
  type LegionIssue,
  type LegionPhase,
  type LegionState,
} from "@legion/contracts/legion-api";
import { z } from "zod";
import type { LegionDaemonClient } from "./daemon-client";

/** The claim a session registered, as the phase ops need it, and as the claim boot
 * (`legion-claim-core`) records it: its own id, the daemon's answer for tree, issue and role, and
 * the secret every grant is minted with. */
export interface ClaimCapability {
  readonly sessionID: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly secret: string;
}

/**
 * A phase worker's operations on the daemon's role-local workflow surface, which every host's
 * `legion` tool serves: pi-legion's (`packages/pi-legion/src/tools.ts`, beside the architect's and
 * the controller's operations) and claude-envoy's MCP tool (`packages/claude-envoy/src/legion-tool.ts`).
 */
export const PHASE_OPS = ["read_record", "handoff_complete", "request_backward_move"] as const;
export type PhaseOp = (typeof PHASE_OPS)[number];

export function isPhaseOp(op: string): op is PhaseOp {
  return (PHASE_OPS as readonly string[]).includes(op);
}

/** A string field `op` requires: present, and not blank. The value is passed on as it was sent. */
const requiredString = (op: PhaseOp, field: string) => {
  const message = `${op} requires ${field}`;
  return z.string({ error: message }).refine((value) => value.trim() !== "", { error: message });
};

/** One operation's arguments: `op` and `shape`, and nothing else. */
const operation = <Op extends PhaseOp, Shape extends z.ZodRawShape>(op: Op, shape: Shape) =>
  z.strictObject(
    { op: z.literal(op), ...shape },
    {
      error: (issue) =>
        issue.code === "unrecognized_keys"
          ? issue.keys.map((key) => `${op} does not accept field "${key}"`).join("; ")
          : undefined,
    }
  );

/**
 * The phase ops' arguments, `op` choosing the operation: what `executePhaseOp` accepts on every
 * host, refusing a call whole before any daemon request, each problem named in the tool's error.
 */
export const PhaseOpArguments = z.discriminatedUnion(
  "op",
  [
    operation("read_record", { issue: requiredString("read_record", "issue") }),
    operation("handoff_complete", {
      summary: requiredString("handoff_complete", "summary"),
      verdict: z
        .enum(["pass", "fail"], { error: "handoff_complete's verdict is pass or fail" })
        .optional(),
      ready: z.boolean({ error: "handoff_complete's ready is true or false" }).optional(),
    }),
    operation("request_backward_move", {
      to: z.enum(LEGION_PHASE_BACKWARD_TARGETS, {
        error: (issue) =>
          typeof issue.input === "string" && issue.input.trim() !== ""
            ? `request_backward_move's to is one of ${LEGION_PHASE_BACKWARD_TARGETS.join(", ")}`
            : "request_backward_move requires to",
      }),
      reason: requiredString("request_backward_move", "reason"),
    }),
  ],
  {
    error: (issue) =>
      issue.code === "invalid_union" ? `op is one of ${PHASE_OPS.join(", ")}` : undefined,
  }
);

/** Every key a member of a union has. */
type KeysOf<T> = T extends unknown ? keyof T : never;

/** The fields the phase ops take, every op's but `op` itself. */
export type PhaseOpField = Exclude<KeysOf<z.input<typeof PhaseOpArguments>>, "op">;

/**
 * The fields of `PhaseOpArguments` as a host declares them in its `legion` tool's one object
 * schema, where `op` chooses the operation: each field once, optional, built with the host's Zod
 * (`zod`: Oh My Pi's `pi.zod`, whose converter reads only schemas its own Zod built, or
 * claude-envoy's), keyed by name so a host lays them out in the order its schema lists them. A
 * field of a kind this does not map throws, so a new field is mapped on purpose.
 */
export function phaseOpParameters(zod: unknown): Record<PhaseOpField, z.ZodType> {
  const api = zodSchemaApi(zod);
  const fields: Record<string, z.ZodType> = {};
  for (const option of PhaseOpArguments.options) {
    for (const [name, declared] of Object.entries(option.shape)) {
      if (name === "op") continue;
      const field = declared instanceof z.ZodOptional ? declared.unwrap() : declared;
      if (field instanceof z.ZodString) fields[name] = api.string().optional();
      else if (field instanceof z.ZodBoolean) fields[name] = api.boolean().optional();
      else if (field instanceof z.ZodEnum) {
        const [first, ...rest] = field.options.map(String);
        if (first === undefined) throw new Error(`phase op field ${name} has no values`);
        fields[name] = api.enum([first, ...rest]).optional();
      } else throw new Error(`phase op field ${name} has a kind phaseOpParameters does not map`);
    }
  }
  // Built from the options' shapes, whose keys are exactly `PhaseOpField`.
  const byField = fields as Record<PhaseOpField, z.ZodType>;
  return byField;
}

/** The `legion` tool's sentences on `read_record` and `request_backward_move`, which a host whose
 * tool states them passes as `before` and `after` (`legionToolDescription`). */
export const PHASE_OP_SENTENCES = {
  readRecord: "read_record returns the record of `issue` from the daemon's state. ",
  requestBackwardMove:
    "request_backward_move asks the daemon to move this issue back to an earlier phase, `to`, for `reason`. ",
} as const;

/**
 * The `legion` tool's description on every host, one template: what the tool is, then `before`,
 * then `handoff_complete`'s contract, then `after`, then where a question for another role goes,
 * `envoyPublish` being the host's name for Envoy's publish tool. `before` and `after` are whole
 * sentences, each ending in a space: the host's sentences on its other operations, and on
 * `read_record` and `request_backward_move` when it states them (`PHASE_OP_SENTENCES`).
 */
export function legionToolDescription(host: {
  readonly envoyPublish: string;
  readonly before?: string;
  readonly after?: string;
}): string {
  return (
    "Perform the workflow operation the Legion daemon assigned this role. The daemon advances phases; this tool cannot spawn workers. " +
    (host.before ?? "") +
    "handoff_complete (phase workers and sub-architects; the daemon accepts a completion only from the role working the issue's current phase) " +
    "reports this phase complete: `summary` (two sentences for the architect, or the merger's READY packet), `verdict` pass|fail from the tester alone " +
    "(the daemon refuses a verdict from any other role), `ready: true` for the merger's READY. A handoff is a committed file, `.legion/<issue>/<phase>.json`: the pushed commit " +
    "carrying it is found here, with the pane's jj, and reported, so write it, commit it and push the branch (`jj git push`) before calling; this call " +
    "refuses, posting nothing, a handoff still in the working copy, missing, not committed on this branch, carried by another pane's commit, or not yet " +
    "on `legion/<issue>@origin`, each naming the remedy. The daemon refuses HANDOFF_NOT_NEW (the commit is the one you reported for your previous " +
    "phase: write and commit this phase's handoff) and, for READY, each refusal the merger's role names; a refused completion changed " +
    "nothing, so fix what it names and complete again. An accepted READY whose `note` begins MANUAL_MERGE_REQUIRED: is one a person merges: arm nothing. " +
    (host.after ?? "") +
    `What a later phase needs goes in your handoff; a question for another live role goes to its role topic with ${host.envoyPublish}.`
  );
}

export interface PhaseOpRuntime {
  readonly daemon: LegionDaemonClient;
  readonly claim: ClaimCapability;
  /** The commit a completion of `phase` by `role` reports (`findHandoffCommit` with the pane's
   * environment): throws naming the remedy when the handoff is not committed and pushed. */
  readonly handoffCommit: (phase: LegionPhase, role: LegionRole) => Promise<string>;
}

/** A grant for one write of the claim's session, minted on `/grants`' session form with the
 * claim's secret: every write of the `legion` tool posts a fresh one with its request. */
export async function mintClaimGrant(
  daemon: LegionDaemonClient,
  claim: ClaimCapability
): Promise<string> {
  const { grantId } = await daemon.grant({
    sessionId: claim.sessionID,
    secret: claim.secret,
    tree: claim.tree,
    issue: claim.issue,
  });
  return grantId;
}

function recordFrom(state: LegionState, issue: string): LegionIssue {
  const record = state.issues[issue];
  if (record === undefined) throw new Error(`The daemon has no record for ${issue}`);
  return record;
}

const ROOT_COMPLETION_REFUSAL =
  "handoff_complete is not available to a root architect session, which runs no phase";

/**
 * Runs one phase op and answers its JSON result: `{ record }` for `read_record`, the daemon's
 * answer for `handoff_complete`, `{}` for `request_backward_move`. A refusal throws before any
 * write, minting nothing and posting nothing: each problem `PhaseOpArguments` finds, joined by
 * `; `, a field the operation does not take named first, then a root architect's completion,
 * then each problem with the operation's own fields in the order it takes them; then a refused
 * handoff-commit lookup. A daemon refusal throws its code and sentence.
 */
export async function executePhaseOp(
  runtime: PhaseOpRuntime,
  input: unknown
): Promise<Readonly<Record<string, unknown>>> {
  const { daemon, claim } = runtime;
  const parsed = PhaseOpArguments.safeParse(input);
  const issues = parsed.success ? [] : parsed.error.issues;
  const unknownFields = issues.filter((issue) => issue.code === "unrecognized_keys");
  const fieldProblems = issues.filter((issue) => issue.code !== "unrecognized_keys");
  const rootCompletion =
    typeof input === "object" &&
    input !== null &&
    "op" in input &&
    input.op === "handoff_complete" &&
    claim.role === "architect" &&
    claim.issue === claim.tree;
  if (rootCompletion) {
    throw new Error(
      [...unknownFields.map((issue) => issue.message), ROOT_COMPLETION_REFUSAL].join("; ")
    );
  }
  if (!parsed.success) {
    throw new Error([...unknownFields, ...fieldProblems].map((issue) => issue.message).join("; "));
  }
  const args = parsed.data;
  switch (args.op) {
    case "read_record":
      return { record: recordFrom(await daemon.state(), args.issue) };
    case "handoff_complete": {
      // The commit is found before the grant is minted: a refusal here posts nothing and mints
      // nothing. The phase is the issue record's, the role the claim's.
      const { phase } = recordFrom(await daemon.state(), claim.issue);
      const commit = await runtime.handoffCommit(phase, claim.role);
      return daemon.handoffComplete({
        grantId: await mintClaimGrant(daemon, claim),
        summary: args.summary,
        verdict: args.verdict ?? "",
        ready: args.ready ?? false,
        commit,
      });
    }
    case "request_backward_move":
      await daemon.phaseBackward({
        grantId: await mintClaimGrant(daemon, claim),
        to: args.to,
        reason: args.reason,
      });
      return {};
  }
}
