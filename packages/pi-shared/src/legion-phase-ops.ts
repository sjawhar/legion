import { type LegionRole, zodSchemaApi } from "@legion/contracts";
import {
  LEGION_PHASE_BACKWARD_TARGETS,
  type LegionIssue,
  type LegionPhase,
  type LegionState,
} from "@legion/contracts/legion-api";
import { z } from "zod";
import type { LegionDaemonClient } from "./daemon-client";
import type { HandoffReport } from "./handoff-record";

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
        .enum(["pass", "pending", "fail"], {
          error: "handoff_complete's verdict is pass, pending or fail",
        })
        .optional(),
      ready: z.boolean({ error: "handoff_complete's ready is true or false" }).optional(),
    }),
    operation("request_backward_move", {
      // The issue the move names: the tree architect's move names the issue it returns from
      // accepting (its grant serves its whole tree); a phase worker's leaves it out, or names its
      // own. Which is the executor's to hold, by the claim's role.
      issue: z.string().optional(),
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
    "request_backward_move returns an issue to an earlier phase (`to`, `reason`): a phase worker moves its own issue; " +
    "the tree's architect moves an issue in accepting, named by `issue`, back to implementing or testing, which counts a review round. ",
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
    "handoff_complete (phase workers and sub-architects; the daemon accepts a completion only from the role working the issue's current phase, " +
    "and the implementer's report on a look of its closed issue's production check) reports this phase complete: `summary` (two sentences for the " +
    "architect, or the merger's READY packet), `verdict` pass|fail from the tester alone on a phase completion (the daemon refuses a verdict from " +
    "any other role there) and pass|pending|fail from the implementer on a production check report (pending: production does not carry the merge " +
    "yet), `ready: true` for the merger's READY. A planning, implementing, testing or reviewing phase ends with a handoff record on the " +
    "Dispatch issue: write it with `dispatch handoff-write --issue <issue> --phase <plan|implement|test|review> --body-file <file>` before " +
    "calling. This call reads the phase's newest record from Dispatch and reports it; the implementer's round also reports the commit " +
    "`legion/<issue>@origin` names, so push the branch first; retro and the merger's READY report the commit the workspace stands on. It " +
    "refuses, posting nothing, a phase with no record, a Dispatch that does not answer, or an implementer's `@-` not yet on " +
    "`legion/<issue>@origin`, each naming the remedy. A production check report " +
    "(the task whose first line is `Production check of <KEY>`, handed to the implementer once the merge closed its issue) needs no handoff and no " +
    "commit: it carries its verdict and summary alone, and the daemon refuses it when no check is open (CHECK_NOT_OPEN). The daemon refuses " +
    "HANDOFF_SESSION_MISMATCH (another session wrote the record), HANDOFF_NOT_NEW (the record is the one you named for your previous phase: " +
    "write this phase's handoff) and, for READY, each " +
    "refusal the merger's role names; a refused completion changed nothing, so fix what it names and complete again. An accepted READY whose `note` " +
    "begins MANUAL_MERGE_REQUIRED: is one a person merges: arm nothing. " +
    (host.after ?? "") +
    `What a later phase needs goes in your handoff; a question for another live role goes to its role topic with ${host.envoyPublish}.`
  );
}

export interface PhaseOpRuntime {
  readonly daemon: LegionDaemonClient;
  readonly claim: ClaimCapability;
  /** What a completion of `phase` by `role` reports beside its summary (`findHandoffReport` with
   * the pane's environment and Dispatch): throws naming the remedy when the phase has no handoff
   * record, Dispatch does not answer, or the implementer's work is not pushed. */
  readonly handoffReport: (phase: LegionPhase, role: LegionRole) => Promise<HandoffReport>;
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
 * then each problem with the operation's own fields in the order it takes them; then what the
 * record decides (a check report's verdict, `pending` on a phase completion, the issue a move
 * names), then a refused handoff report (`findHandoffReport`). A daemon refusal throws its code
 * and sentence.
 *
 * A completion is a phase's or the implementer's report on a look of its closed issue's
 * production check, told apart as the daemon tells them (`api.handoffComplete`): the
 * implementer's call on an issue no role works (`done`) whose `check` is open. A check report
 * names its verdict (`pass`, `pending` or `fail`) and nothing else: no handoff was written and
 * nothing pushed, so no record or commit is looked up and none is sent; `pending` is a check's
 * verdict alone, refused on a phase completion.
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
      // The record is read before anything is minted: it says whether this is a phase
      // completion or a check report, by the daemon's own rule (`api/handoff.go`): the
      // implementer's call on an issue no role works (`done`) is a check report, whether or not
      // the record shows a check — a done issue whose check closed, or opened none, has no
      // handoff for the implementer to write, so no record or commit is looked up, and the daemon
      // answers the report `CHECK_NOT_OPEN`. The report's own refusals (a verdict, no READY) apply
      // while the record shows the check it is of. A refusal here posts nothing and mints nothing.
      const record = recordFrom(await daemon.state(), claim.issue);
      if (claim.role === "implementer" && record.phase === "done") {
        if (record.check !== undefined) {
          if (args.verdict === undefined) {
            throw new Error(
              "handoff_complete on a production check names a verdict of pass, pending or fail"
            );
          }
          if (args.ready === true) {
            throw new Error("handoff_complete on a production check is no READY: leave ready out");
          }
        }
        return daemon.handoffComplete({
          grantId: await mintClaimGrant(daemon, claim),
          summary: args.summary,
          verdict: args.verdict ?? "",
          ready: false,
        });
      }
      if (args.verdict === "pending") {
        throw new Error(
          "handoff_complete's verdict is pass or fail on a phase completion; pending is a production check's"
        );
      }
      // The record and the commit are found before the grant is minted. The phase is the issue
      // record's, the role the claim's.
      const report = await runtime.handoffReport(record.phase, claim.role);
      return daemon.handoffComplete({
        grantId: await mintClaimGrant(daemon, claim),
        summary: args.summary,
        verdict: args.verdict ?? "",
        ready: args.ready ?? false,
        ...report,
      });
    }
    case "request_backward_move": {
      // An architect's grant serves its whole tree, so its move names the issue it returns from
      // accepting. A phase worker's grant names its own issue, the only one it may move, so its
      // request leaves `issue` out; one naming another issue is refused here, as the daemon would
      // (ISSUE_NOT_GRANTS).
      let issue: string | undefined;
      if (claim.role === "architect") {
        if (args.issue === undefined || args.issue.trim() === "") {
          throw new Error("request_backward_move requires issue");
        }
        issue = args.issue;
      } else if (args.issue !== undefined && args.issue !== claim.issue) {
        throw new Error(
          `a phase worker moves only its own issue ${claim.issue}, not ${args.issue}`
        );
      }
      await daemon.phaseBackward({
        grantId: await mintClaimGrant(daemon, claim),
        to: args.to,
        reason: args.reason,
        ...(issue === undefined ? {} : { issue }),
      });
      return {};
    }
  }
}
