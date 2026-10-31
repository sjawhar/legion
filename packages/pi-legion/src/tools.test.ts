import { expect, test } from "bun:test";
import type { LegionRole } from "@legion/contracts";
import {
  LEGION_PHASES,
  LEGION_WORKFLOW_PHASES,
  type LegionPhase,
  type LegionState,
  LegionStateResponse,
} from "@legion/contracts/legion-api";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import { findHandoffCommit, type JjRunner } from "@legion/pi-shared/handoff-commit";
import type { PiApi, RegisteredTool, SessionContext } from "@legion/pi-shared/pi-types";
import { z } from "zod";
import { createLegionTool, type LegionToolSession } from "./tools";

function context(sessionId = "ses_208"): SessionContext {
  return {
    cwd: "/workspace",
    hasUI: true,
    setInterval: () => undefined,
    setTimeout: () => undefined,
    sessionManager: {
      getSessionId: () => sessionId,
      getSessionFile: () => "/sessions/208.jsonl",
      ensureOnDisk: async () => undefined,
      getEntries: () => [],
    },
    ui: { notify: () => undefined },
  };
}

const pi = {
  zod: z,
  sendMessage: () => undefined,
  appendEntry: () => undefined,
  getActiveTools: () => [],
  setActiveTools: async () => undefined,
  on: () => undefined,
  registerTool: () => undefined,
  registerCommand: () => undefined,
  registerMessageRenderer: () => undefined,
} as unknown as PiApi;

/** The document lookup of a tool that must never look a document up. */
const noDocumentLookup = async (_issue: string, reference: string): Promise<string> => {
  throw new Error(`no document lookup expected for "${reference}"`);
};

/** The controller grant of a tool that answers for no controller. */
const noControllerGrant = async (sessionId: string): Promise<never> => {
  throw new Error(`no controller grant expected for ${sessionId}`);
};

/** The handoff-commit lookup of a tool that must never complete a phase. */
const noHandoffCommit = async (phase: LegionPhase, role: LegionRole): Promise<never> => {
  throw new Error(`no handoff-commit lookup expected for the ${role} at ${phase}`);
};

/** A claim's session on LEGION-208: a root architect's, a sub-architect's (its issue is not its
 * tree), or a phase worker's by role. */
function claimSession(role: LegionRole, issue = "LEGION-208"): LegionToolSession {
  return {
    kind: role === "architect" ? "architect" : "phase-worker",
    role,
    sessionID: "ses_208",
    tree: "LEGION-208",
    issue,
    secret: "claim-secret",
  };
}

/** A daemon stub that records every call: `grant` answers a grant, `state` the state given, and
 * every other route the answer given for it (an empty object by default). */
function recordingDaemon(options: {
  readonly state?: unknown;
  readonly answers?: Readonly<Record<string, unknown>>;
}) {
  const calls: Array<readonly [string, object | undefined]> = [];
  const routes = [
    "gateRegister",
    "waveRelease",
    "phaseBackward",
    "phaseRetry",
    "signOff",
    "rootClose",
    "childPark",
    "childRerun",
    "issueStatus",
    "handoffComplete",
  ];
  const daemon = {
    grant: async (input: object) => {
      calls.push(["grant", input]);
      return { grantId: "grant-208", expiresAt: "2099-01-01T00:00:00Z" };
    },
    state: async () => {
      calls.push(["state", undefined]);
      return options.state ?? { issues: {} };
    },
    ...Object.fromEntries(
      routes.map((name) => [
        name,
        async (input: object) => {
          calls.push([name, input]);
          const answer = options.answers?.[name];
          if (answer instanceof Error) throw answer;
          return answer ?? {};
        },
      ])
    ),
  };
  return { calls, daemon: () => daemon as never };
}

/** The `legion` tool description Oh My Pi agents read, written out whole: the shared template
 * (`legionToolDescription` in `@legion/pi-shared/legion-phase-ops`) must reproduce it exactly. */
const OMP_LEGION_TOOL_DESCRIPTION =
  "Perform the workflow operation the Legion daemon assigned this role. The daemon advances phases; this tool cannot spawn workers. " +
  "handoff_complete (phase workers and sub-architects; the daemon accepts a completion only from the role working the issue's current phase, " +
  "and the implementer's report on a look of its closed issue's production check) reports this phase complete: `summary` (two sentences for the " +
  "architect, or the merger's READY packet), `verdict` pass|fail from the tester alone on a phase completion (the daemon refuses a verdict from " +
  "any other role there) and pass|pending|fail from the implementer on a production check report (pending: production does not carry the merge " +
  "yet), `ready: true` for the merger's READY. A handoff is a committed " +
  "file, `.legion/<issue>/<phase>.json`: the pushed commit carrying it is found here, with the pane's jj, and reported, so write it, commit it and " +
  "push the branch (`jj git push`) before calling; this call refuses, posting nothing, a handoff still in the working copy, missing, not committed " +
  "on this branch, carried by another pane's commit, or not yet on `legion/<issue>@origin`, each naming the remedy. A production check report " +
  "(the task whose first line is `Production check of <KEY>`, handed to the implementer once the merge closed its issue) needs no handoff and no " +
  "commit: it carries its verdict and summary alone, and the daemon refuses it when no check is open (CHECK_NOT_OPEN). The daemon refuses " +
  "HANDOFF_NOT_NEW (the commit is the one you reported for your previous phase: write and commit this phase's handoff), the implementer's " +
  "completion of implementing with no open pull request on `legion/<issue>` and none recorded merged (HANDOFF_NO_PULL_REQUEST: open it; one " +
  "naming a closed pull request is the architect's to decide) and, for READY, each " +
  "refusal the merger's role names; a refused completion changed nothing, so fix what it names and complete again. An accepted READY whose `note` " +
  "begins MANUAL_MERGE_REQUIRED: is one a person merges: arm nothing. request_backward_move returns an issue to an earlier phase (`to`, `reason`): " +
  "a phase worker moves its own issue; the tree's architect moves an issue in accepting, named by `issue`, back to implementing or testing, which " +
  "counts a review round. sign_off is the architect's acceptance of an issue in accepting, moving it to retro; the daemon refuses it in every " +
  "other phase (SIGNOFF_OUTSIDE_ACCEPTING), and issues still running beneath it do not hold it: a root's close waits " +
  "for them instead. " +
  "The controller's read_state with no field returns a summary of the daemon's whole state — the project, admission, each " +
  "capability, one line per recorded issue, and under each issue Legion still runs its slot, hold, gate and each claim's " +
  "state with its last turn times — and with issue, issues, phase or settings returns the full records it names; " +
  "set_status moves an issue to todo, backlog or icebox. " +
  "What a later phase needs goes in your handoff; a question for another live role goes to its role topic with envoy_publish.";

/** Every session kind the tool answers for: a root architect, a sub-architect, each phase worker,
 * and the controller. */
const EVERY_SESSION: readonly LegionToolSession[] = [
  claimSession("architect"),
  claimSession("architect", "LEGION-209"),
  ...(["planner", "implementer", "tester", "reviewer", "merger"] as const).map((role) =>
    claimSession(role)
  ),
  { kind: "controller", sessionID: "ses_controller" },
];

const toolFor = (active: LegionToolSession) =>
  createLegionTool({
    pi,
    daemon: recordingDaemon({}).daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    handoffCommit: noHandoffCommit,
    resolveDocument: noDocumentLookup,
    session: () => active,
  });

test("the legion tool's description is the exact text Oh My Pi agents read, for every session", () => {
  for (const active of EVERY_SESSION) {
    expect(toolFor(active).description).toBe(OMP_LEGION_TOOL_DESCRIPTION);
  }
});

/** The `legion` tool's parameter schema as Oh My Pi agents are given it, serialized whole: every
 * property and every `op` value in the order the tool declares them. */
const OMP_LEGION_TOOL_SCHEMA = JSON.stringify({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  properties: {
    op: {
      type: "string",
      enum: [
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
        "read_state",
        "set_status",
      ],
    },
    issue: { type: "string" },
    artifactId: {
      type: "string",
      description:
        "register_gate's root spec document: its artifact id, slug, or filename, as a dispatch command's --artifact takes it",
    },
    version: { type: "number" },
    issues: { type: "array", items: { type: "string" } },
    to: {
      type: "string",
      enum: [
        "admitted",
        "planning",
        "implementing",
        "testing",
        "reviewing",
        "accepting",
        "retro",
        "merging",
        "done",
        "held",
      ],
    },
    reason: { type: "string" },
    decision: { type: "string", enum: ["retry", "escalate"] },
    summary: { type: "string" },
    verdict: { type: "string", enum: ["pass", "pending", "fail"] },
    ready: { type: "boolean" },
    status: { type: "string", enum: ["todo", "backlog", "icebox"] },
    phase: { type: "string", enum: [...LEGION_PHASES] },
    settings: { type: "boolean" },
  },
  required: ["op"],
  additionalProperties: false,
});

test("the legion tool's parameter schema serializes to the exact schema Oh My Pi agents are given, for every session", () => {
  for (const active of EVERY_SESSION) {
    expect(JSON.stringify(z.toJSONSchema(toolFor(active).parameters as z.ZodType))).toBe(
      OMP_LEGION_TOOL_SCHEMA
    );
  }
});

test("a root architect's handoff_complete is refused as unavailable before its arguments are read", async () => {
  const root = toolFor(claimSession("architect"));
  for (const parameters of [
    {},
    { summary: "" },
    { summary: "x", verdict: "maybe", ready: "yes" },
  ]) {
    await expect(
      root.execute("", { op: "handoff_complete", ...parameters }, undefined, undefined, context())
    ).resolves.toEqual({
      content: [
        {
          type: "text",
          text: "handoff_complete is not available to a root architect session, which runs no phase",
        },
      ],
      details: {},
      isError: true,
    });
  }
  // A field handoff_complete never takes is named first, as the operation's field check names it.
  await expect(
    root.execute("", { op: "handoff_complete", phase: "plan" }, undefined, undefined, context())
  ).resolves.toMatchObject({
    content: [
      {
        type: "text",
        text: 'handoff_complete does not accept field "phase"; handoff_complete is not available to a root architect session, which runs no phase',
      },
    ],
  });
});

test("the legion tool exposes only the workflow operations each role owns", async () => {
  const state = {
    issues: { "LEGION-208": { key: "LEGION-208", phase: "held" } },
  };
  const { calls, daemon } = recordingDaemon({ state });
  const run = async (session: LegionToolSession, parameters: Record<string, unknown>) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      handoffCommit: noHandoffCommit,
      resolveDocument: noDocumentLookup,
      session: () => session,
    }).execute("", parameters, undefined, undefined, context());
  const architect = claimSession("architect");
  const worker = claimSession("implementer");
  const phaseWorkerTool = createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    handoffCommit: noHandoffCommit,
    resolveDocument: noDocumentLookup,
    session: () => worker,
  });

  await expect(
    run(architect, {
      op: "register_gate",
      issue: "LEGION-208",
      artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
      version: 7,
    })
  ).resolves.toMatchObject({ details: {} });
  expect(calls).toEqual([
    [
      "grant",
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-208" },
    ],
    [
      "gateRegister",
      {
        grantId: "grant-208",
        issue: "LEGION-208",
        artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
        version: 7,
      },
    ],
  ]);

  expect(
    (phaseWorkerTool.parameters as z.ZodType).safeParse({
      op: "request_backward_move",
      to: "integrating",
      reason: "test failed",
    }).success
  ).toBeFalse();

  // A phase worker's move is of its own issue, which its grant names, so the request carries no
  // issue; one naming another issue is refused before any mint. The architect's grant serves its
  // whole tree, so its move names the issue it returns from accepting, and is refused without one.
  await expect(
    run(worker, { op: "request_backward_move", to: "implementing", reason: "test failed" })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseBackward",
    { grantId: "grant-208", to: "implementing", reason: "test failed" },
  ]);
  await expect(
    run(worker, {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: "LEGION-208",
    })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseBackward",
    { grantId: "grant-208", to: "implementing", reason: "test failed" },
  ]);
  const beforeRefusedMove = calls.length;
  await expect(
    run(worker, {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: "LEGION-209",
    })
  ).resolves.toMatchObject({
    isError: true,
    content: [
      {
        type: "text",
        text: expect.stringContaining(
          "a phase worker moves only its own issue LEGION-208, not LEGION-209"
        ),
      },
    ],
  });
  await expect(
    run(architect, { op: "request_backward_move", to: "testing", reason: "the proof is thin" })
  ).resolves.toMatchObject({
    isError: true,
    content: [
      { type: "text", text: expect.stringContaining("request_backward_move requires issue") },
    ],
  });
  expect(calls).toHaveLength(beforeRefusedMove);
  await expect(
    run(architect, {
      op: "request_backward_move",
      to: "testing",
      reason: "the proof is thin",
      issue: "LEGION-209",
    })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseBackward",
    { grantId: "grant-208", to: "testing", reason: "the proof is thin", issue: "LEGION-209" },
  ]);
  await expect(
    run(architect, { op: "release_children", issues: ["LEGION-209"] })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual(["waveRelease", { grantId: "grant-208", issues: ["LEGION-209"] }]);
  await expect(
    run(architect, { op: "retry_or_escalate", issue: "LEGION-208", decision: "retry" })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseRetry",
    { grantId: "grant-208", issue: "LEGION-208", decision: "retry" },
  ]);
  await expect(run(architect, { op: "sign_off", issue: "LEGION-208" })).resolves.toMatchObject({
    details: {},
  });
  expect(calls.at(-1)).toEqual(["signOff", { grantId: "grant-208", issue: "LEGION-208" }]);
  await expect(
    run(architect, { op: "close_root", issue: "LEGION-208", reason: "no change" })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "rootClose",
    { grantId: "grant-208", issue: "LEGION-208", reason: "no change" },
  ]);
  await expect(run(architect, { op: "close_root", issue: "LEGION-208" })).resolves.toMatchObject({
    isError: true,
  });
  await expect(run(architect, { op: "park_child", issue: "LEGION-209" })).resolves.toMatchObject({
    details: {},
  });
  expect(calls.at(-1)).toEqual(["childPark", { grantId: "grant-208", issue: "LEGION-209" }]);
  await expect(run(architect, { op: "rerun_child", issue: "LEGION-209" })).resolves.toMatchObject({
    details: {},
  });
  expect(calls.at(-1)).toEqual(["childRerun", { grantId: "grant-208", issue: "LEGION-209" }]);
  await expect(run(architect, { op: "read_record", issue: "LEGION-208" })).resolves.toMatchObject({
    details: { record: state.issues["LEGION-208"] },
  });
  for (const op of ["sign_off", "close_root", "park_child", "rerun_child"]) {
    await expect(run(worker, { op, issue: "LEGION-208" })).resolves.toMatchObject({
      isError: true,
    });
  }
  // The controller's operations belong to no claim, and nothing schedules a worker.
  for (const session of [architect, worker]) {
    await expect(run(session, { op: "read_state" })).resolves.toMatchObject({
      isError: true,
      content: [{ type: "text", text: `read_state is not available to a ${session.kind} session` }],
    });
    await expect(
      run(session, { op: "set_status", issue: "LEGION-208", status: "todo" })
    ).resolves.toMatchObject({ isError: true });
  }
  await expect(run(architect, { op: "spawn_worker", issue: "LEGION-208" })).resolves.toMatchObject({
    isError: true,
  });
});

// Every role reads one description, and the daemon accepts a verdict from the tester alone
// (VERDICT_ROLE_FORBIDDEN): it may not leave another role to infer from its own instructions that
// it reports one.
test("the legion tool's description gives a verdict to the tester alone", () => {
  const tool = createLegionTool({
    pi,
    daemon: recordingDaemon({}).daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    handoffCommit: noHandoffCommit,
    resolveDocument: noDocumentLookup,
    session: () => claimSession("reviewer"),
  });
  expect(tool.description).toContain("`verdict` pass|fail from the tester alone");
  expect(tool.description).not.toContain("when your role's instructions require one");
});

test("a workflow refusal tells the agent both its code and message", async () => {
  const refusal = Object.assign(
    new Error(
      "POST /legion/v1/signoff failed with 409 SIGNOFF_OUTSIDE_ACCEPTING: LEGION-208 is in phase reviewing, not accepting; this sign-off changed nothing"
    ),
    {
      code: "SIGNOFF_OUTSIDE_ACCEPTING",
      detail: "LEGION-208 is in phase reviewing, not accepting; this sign-off changed nothing",
    }
  );
  const tool = createLegionTool({
    pi,
    daemon: (() => ({
      grant: async () => ({ grantId: "grant-208" }),
      signOff: async () => Promise.reject(refusal),
    })) as never,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    handoffCommit: noHandoffCommit,
    resolveDocument: noDocumentLookup,
    session: () => claimSession("architect"),
  });

  await expect(
    tool.execute("", { op: "sign_off", issue: "LEGION-208" }, undefined, undefined, context())
  ).resolves.toMatchObject({
    content: [{ type: "text", text: expect.stringContaining("SIGNOFF_OUTSIDE_ACCEPTING") }],
    isError: true,
    details: {},
  });
});

test("register_gate takes the document reference the Dispatch tools take, and registers its id", async () => {
  const spec = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093";
  const registered: unknown[] = [];
  const resolved: Array<readonly [string, string]> = [];
  const daemon = () =>
    ({
      grant: async () => ({ grantId: "grant-208" }),
      gateRegister: async (input: object) => {
        registered.push(input);
        return {};
      },
    }) as never;
  const run = (artifactId: string, issue = "LEGION-208", sessionIssue = "LEGION-208") =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      handoffCommit: noHandoffCommit,
      resolveDocument: async (issue: string, reference: string) => {
        resolved.push([issue, reference]);
        if (reference === "notes") throw new Error(`No document "notes" on ${issue}`);
        return spec;
      },
      session: () => claimSession("architect", sessionIssue),
    }).execute(
      "",
      { op: "register_gate", issue, artifactId, version: 2 },
      undefined,
      undefined,
      context()
    );
  const registration = { grantId: "grant-208", issue: "LEGION-208", artifactId: spec, version: 2 };

  // A reference is looked up and its id registered; an id goes to the daemon as it is.
  await expect(run("spec")).resolves.toMatchObject({ details: {} });
  await expect(run(spec)).resolves.toMatchObject({ details: {} });
  expect(registered).toEqual([registration, registration]);
  expect(resolved).toEqual([["LEGION-208", "spec"]]);

  // A reference the lookup refuses registers nothing. So does a call from anyone but the tree
  // root's own architect, refused before any lookup: a sub-architect, or the root architect
  // naming another issue.
  const root = "the design gate belongs to the tree root LEGION-208";
  const refusals = [
    [await run("notes"), 'No document \\"notes\\" on LEGION-208'],
    [await run("spec", "LEGION-208", "LEGION-209"), `${root}; its root architect registers it`],
    [await run("spec", "LEGION-209"), `${root}; register it there`],
  ] as const;
  for (const [result, message] of refusals) {
    expect(result.isError).toBe(true);
    expect(JSON.stringify(result.content)).toContain(message);
  }
  expect(registered).toHaveLength(2);
  expect(resolved).toEqual([
    ["LEGION-208", "spec"],
    ["LEGION-208", "notes"],
  ]);
});

/** The pane a completion runs in: its workspace and issue, and the implement App's identity. */
const paneEnv = {
  LEGION_WORKSPACE: "/workspaces/LEGION-208",
  LEGION_ISSUE: "LEGION-208",
  JJ_USER: "legion-implementer[bot]",
  JJ_EMAIL: "271566630+legion-implementer[bot]@users.noreply.github.com",
};
const CARRYING = "210d53a9d1b109df97fbb5dd5041d659dbab1323";
const STANDING = "c0de0000000000000000000000000000000000ff";

/** A jj that answers the lookup's commands for a handoff committed, authored by this pane and
 * pushed, unless `answers` says otherwise for one of them. */
function paneJj(answers: {
  readonly uncommitted?: string;
  readonly listed?: string;
  readonly carrying?: string;
  readonly author?: string;
  readonly pushed?: string;
}): JjRunner {
  return async (args) => {
    const [command, , revision] = args;
    if (command === "log" && revision === "@-") return STANDING;
    if (command === "diff") return answers.uncommitted ?? "";
    if (command === "file") return answers.listed ?? ".legion/LEGION-208/implement.json";
    if (command === "log" && revision?.startsWith("latest(")) return answers.carrying ?? CARRYING;
    if (command === "log" && revision === CARRYING) {
      return answers.author ?? `${paneEnv.JJ_USER}\n${paneEnv.JJ_EMAIL}`;
    }
    if (command === "log" && revision?.includes("remote_bookmarks")) {
      return answers.pushed ?? CARRYING;
    }
    throw new Error(`unexpected jj ${args.join(" ")}`);
  };
}

/** A daemon state with LEGION-208 at `phase` (and LEGION-209, a sub-architect's issue, admitted). */
const stateAt = (phase: LegionPhase) => ({
  issues: {
    "LEGION-208": { key: "LEGION-208", phase },
    "LEGION-209": { key: "LEGION-209", phase: "admitted" },
  },
});

test("handoff_complete finds the pushed commit carrying the handoff, posts it with a fresh grant, closes the phase on success, and answers the daemon's note", async () => {
  const completed: string[] = [];
  const { calls, daemon } = recordingDaemon({
    state: stateAt("implementing"),
    answers: {
      handoffComplete: {
        note: "pull request #312 is already merged, so READY was published without reading its head's handoffs",
      },
    },
  });
  const run = (
    session: LegionToolSession,
    parameters: Record<string, unknown>,
    jj: JjRunner = paneJj({})
  ) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
      handoffCommit: (phase, role) => findHandoffCommit({ phase, role, env: paneEnv, jj }),
      resolveDocument: noDocumentLookup,
      session: () => session,
    }).execute("", parameters, undefined, undefined, context());

  // The implementer at implementing: the issue's phase is read from the daemon's state, the
  // commit carrying .legion/LEGION-208/implement.json is found in the pane, and the request
  // carries it with the wire's zero values for the verdict and READY.
  await expect(
    run(claimSession("implementer"), { op: "handoff_complete", summary: "Done." })
  ).resolves.toEqual({
    content: [
      {
        type: "text",
        text: '{"note":"pull request #312 is already merged, so READY was published without reading its head\'s handoffs"}',
      },
    ],
    details: {
      note: "pull request #312 is already merged, so READY was published without reading its head's handoffs",
    },
  });
  expect(calls).toEqual([
    ["state", undefined],
    [
      "grant",
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-208" },
    ],
    [
      "handoffComplete",
      { grantId: "grant-208", summary: "Done.", verdict: "", ready: false, commit: CARRYING },
    ],
  ]);
  expect(completed).toEqual(["ses_208"]);

  // A role that does not work the issue's phase reports the commit the workspace stands on, and
  // the daemon refuses it naming whose phase it is: the tester at implementing, the merger's
  // READY, and a sub-architect, each posting @-. The verdict and READY travel as given.
  await run(claimSession("tester"), { op: "handoff_complete", summary: "Red.", verdict: "fail" });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    { grantId: "grant-208", summary: "Red.", verdict: "fail", ready: false, commit: STANDING },
  ]);
  await run(claimSession("merger"), { op: "handoff_complete", summary: "READY", ready: true });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    { grantId: "grant-208", summary: "READY", verdict: "", ready: true, commit: STANDING },
  ]);
  await run(claimSession("architect", "LEGION-209"), {
    op: "handoff_complete",
    summary: "Children released.",
  });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    {
      grantId: "grant-208",
      summary: "Children released.",
      verdict: "",
      ready: false,
      commit: STANDING,
    },
  ]);
  expect(completed).toEqual(["ses_208", "ses_208", "ses_208", "ses_208"]);

  // Refused before any daemon call: a root architect, which runs no phase; a missing summary; a
  // verdict or a ready of the wrong shape; and a phase, which the daemon knows from the grant.
  const before = calls.length;
  const refusals = [
    [claimSession("architect"), { summary: "Done." }, "root architect session"],
    [claimSession("implementer"), {}, "handoff_complete requires summary"],
    [
      claimSession("tester"),
      { summary: "x", verdict: "maybe" },
      "verdict is pass, pending or fail",
    ],
    [claimSession("merger"), { summary: "x", ready: "yes" }, "ready is true or false"],
    [claimSession("planner"), { summary: "x", phase: "plan" }, 'does not accept field "phase"'],
  ] as const;
  for (const [session, parameters, message] of refusals) {
    const result = await run(session, { op: "handoff_complete", ...parameters });
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
  expect(calls).toHaveLength(before);
  expect(completed).toHaveLength(4);

  // `pending` is a production check's verdict: on a phase completion it is refused once the record
  // says the completion is a phase's, with nothing minted or posted.
  const pendingOnPhase = await run(claimSession("tester"), {
    op: "handoff_complete",
    summary: "Waiting on the deploy.",
    verdict: "pending",
  });
  expect(pendingOnPhase.isError).toBe(true);
  expect(pendingOnPhase.content).toEqual([
    {
      type: "text",
      text: "handoff_complete's verdict is pass or fail on a phase completion; pending is a production check's",
    },
  ]);
  expect(calls.at(-1)).toEqual(["state", undefined]);
  expect(completed).toHaveLength(4);

  // Each lookup refusal is the tool's error, naming the remedy: the state is read, and then
  // nothing is minted or posted, and the phase stays open.
  const lookupRefusals = [
    [
      paneJj({ uncommitted: ".legion/LEGION-208/implement.json" }),
      ".legion/LEGION-208/implement.json has changes in the working copy that are not committed: commit this phase's handoff before completing",
    ],
    [
      paneJj({ listed: "" }),
      ".legion/LEGION-208/implement.json is missing from the workspace: write this phase's handoff, then commit and push it before completing",
    ],
    [
      paneJj({ carrying: "" }),
      ".legion/LEGION-208/implement.json is not committed on this issue's branch (only the base branch carries it): write and commit this phase's handoff",
    ],
    [
      paneJj({ author: "legion-reviewer[bot]\nr@example.invalid" }),
      `.legion/LEGION-208/implement.json is carried by commit ${CARRYING}, authored by legion-reviewer[bot] <r@example.invalid>, not this pane's legion-implementer[bot]: run jj new, then write and commit this phase's handoff again`,
    ],
    [
      paneJj({ pushed: "" }),
      `.legion/LEGION-208/implement.json is carried by commit ${CARRYING}, which is not on legion/LEGION-208@origin: push the issue branch, then complete again`,
    ],
  ] as const;
  for (const [jj, message] of lookupRefusals) {
    const result = await run(
      claimSession("implementer"),
      { op: "handoff_complete", summary: "Done." },
      jj
    );
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: message }]);
    expect(calls.at(-1)).toEqual(["state", undefined]);
  }
  expect(completed).toHaveLength(4);
});

/** LEGION-208 closed by its merge, with its production check open on its second look. */
const openCheck = {
  repo: "acme/widgets",
  pullRequest: 1239,
  mergeCommit: "3e9c0a7b5d2f4e8a1c6b9d0f2a4e7c5b8d1f3a60",
  openedAt: "2026-09-22T08:40:12Z",
  looks: 2,
  lookOpen: true,
  lastWake: {
    kind: "issue.deployed",
    detail: "run 38230245571 completed 2026-09-22T09:10:44Z",
    at: "2026-09-22T09:10:51Z",
  },
  nextWakeAt: "2026-10-06T08:40:12Z",
  last: false,
};

test("the implementer's report on its closed issue's production check carries a verdict and no commit", async () => {
  const completed: string[] = [];
  const { calls, daemon } = recordingDaemon({
    state: {
      issues: {
        "LEGION-208": { key: "LEGION-208", phase: "done", check: openCheck },
      },
    },
  });
  const run = (session: LegionToolSession, parameters: Record<string, unknown>) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
      // A check report resolves no commit: the lookup is never asked.
      handoffCommit: noHandoffCommit,
      resolveDocument: noDocumentLookup,
      session: () => session,
    }).execute("", parameters, undefined, undefined, context());

  // Each of the three verdicts is posted with a fresh grant, the summary and no commit, and the
  // check task's turn is closed as a phase's would be.
  for (const verdict of ["pending", "pass", "fail"]) {
    await expect(
      run(claimSession("implementer"), {
        op: "handoff_complete",
        summary: `Look 2: ${verdict}.`,
        verdict,
      })
    ).resolves.toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
    expect(calls.slice(-3)).toEqual([
      ["state", undefined],
      [
        "grant",
        { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-208" },
      ],
      [
        "handoffComplete",
        { grantId: "grant-208", summary: `Look 2: ${verdict}.`, verdict, ready: false },
      ],
    ]);
  }
  expect(completed).toEqual(["ses_208", "ses_208", "ses_208"]);

  // Refused after the record is read, with nothing minted or posted: a report with no verdict,
  // and one sent as READY.
  const refusals = [
    [
      { summary: "Looked." },
      "handoff_complete on a production check names a verdict of pass, pending or fail",
    ],
    [
      { summary: "Looked.", verdict: "pass", ready: true },
      "handoff_complete on a production check is no READY: leave ready out",
    ],
  ] as const;
  for (const [parameters, message] of refusals) {
    const result = await run(claimSession("implementer"), {
      op: "handoff_complete",
      ...parameters,
    });
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: message }]);
    expect(calls.at(-1)).toEqual(["state", undefined]);
  }
  expect(completed).toHaveLength(3);

  // Only the implementer reports on the check: any other role's completion on the closed issue is
  // a phase completion, which looks its commit up (and the daemon then refuses it naming whose
  // phase it is).
  const { daemon: testerDaemon } = recordingDaemon({
    state: { issues: { "LEGION-208": { key: "LEGION-208", phase: "done", check: openCheck } } },
  });
  const testerAtDone = await createLegionTool({
    pi,
    daemon: testerDaemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    handoffCommit: noHandoffCommit,
    resolveDocument: noDocumentLookup,
    session: () => claimSession("tester"),
  }).execute(
    "",
    { op: "handoff_complete", summary: "Looked.", verdict: "pass" },
    undefined,
    undefined,
    context()
  );
  expect(testerAtDone.isError).toBe(true);
  expect(testerAtDone.content).toEqual([
    { type: "text", text: "no handoff-commit lookup expected for the tester at done" },
  ]);

  // The implementer's completion on a done issue is a check report whatever the record shows, as
  // the daemon reads it (api/handoff.go): once the check closed, or when none opened, no commit is
  // looked up and the report is posted as given, for the daemon's one refusal, CHECK_NOT_OPEN.
  const { calls: closedCalls, daemon: closedDaemon } = recordingDaemon({
    state: { issues: { "LEGION-208": { key: "LEGION-208", phase: "done" } } },
  });
  const implementerAtDone = await createLegionTool({
    pi,
    daemon: closedDaemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    handoffCommit: noHandoffCommit,
    resolveDocument: noDocumentLookup,
    session: () => claimSession("implementer"),
  }).execute("", { op: "handoff_complete", summary: "Looked." }, undefined, undefined, context());
  expect(implementerAtDone.isError).toBeUndefined();
  expect(closedCalls.at(-1)).toEqual([
    "handoffComplete",
    { grantId: "grant-208", summary: "Looked.", verdict: "", ready: false },
  ]);
});

test("a refused completion surfaces the daemon's code and leaves the phase open", async () => {
  const refusal = Object.assign(
    new Error(
      `POST /legion/v1/handoff/complete failed with 409 HANDOFF_NOT_NEW: the implementer reported commit ${CARRYING} for its previous phase of LEGION-208; write and commit this phase's handoff before completing`
    ),
    { code: "HANDOFF_NOT_NEW" }
  );
  const completed: string[] = [];
  const { daemon } = recordingDaemon({
    state: stateAt("implementing"),
    answers: { handoffComplete: refusal },
  });
  const tool = createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
    handoffCommit: (phase, role) =>
      findHandoffCommit({ phase, role, env: paneEnv, jj: paneJj({}) }),
    resolveDocument: noDocumentLookup,
    session: () => claimSession("implementer"),
  });

  await expect(
    tool.execute("", { op: "handoff_complete", summary: "Done." }, undefined, undefined, context())
  ).resolves.toMatchObject({
    isError: true,
    content: [{ type: "text", text: expect.stringContaining("HANDOFF_NOT_NEW") }],
  });
  expect(completed).toEqual([]);
});

/** A state the contracts schema accepts, with what a test varies laid over it: `read_state`
 * summarizes the daemon's own answer, so the tests build it as the daemon serves it. */
function legionState(overrides: Record<string, unknown> = {}): LegionState {
  return LegionStateResponse.parse({
    daemon: {
      project: "INFRA",
      schemaVersion: 71,
      boots: 9,
      firstBootAt: "2026-10-01T00:00:00Z",
      startedAt: "2026-10-30T01:02:03Z",
    },
    admission: { cap: 8, active: [], waiting: [], blocked: [], free: 0 },
    issues: {},
    pendingStatusWrites: [],
    heldOutboxRows: [],
    strandedClaims: [],
    capabilities: [],
    repositories: { owners: ["acme"], mapped: [], refused: [], withoutWebhookApp: [], caches: [] },
    consumers: [],
    settings: { review_round_cap: 3, worker_boot_timeout_seconds: 900 },
    ...overrides,
  });
}

/** The phase workers of an issue, in the order the summary lists them under the architect. */
const WORKER_ROLES = ["planner", "implementer", "tester", "reviewer", "merger"] as const;

/** The state at the size that cut the controller's read (dispatch://INFRA-329): the 92 issues
 * the INFRA deployment recorded, `running` of them still in the workflow, each carrying a slot, a
 * design gate, a pull request, an architect and a claim for every role with both turn times. Half
 * the issues run at once, nearly six times the deployment's admission cap, so the summary is
 * measured well above the load it meets. */
function stateOf92Issues(running = 46): LegionState {
  const at = (minutes: number) =>
    `2026-10-30T${String(Math.floor(minutes / 60)).padStart(2, "0")}:${String(minutes % 60).padStart(2, "0")}:00Z`;
  const claimView = (index: number, state: string) => ({
    session: `ses_${index}`,
    state,
    locator: {
      runtime: "sandbox",
      claim: `claim-${index}`,
      incarnation: `uid-${index}/1`,
      sandbox: {
        namespace: "legion",
        name: `infra-${index}`,
        podUid: `uid-${index}`,
        container: "roles",
        generation: 1,
      },
    },
    pluginContract: 42,
    lastTurnStart: at(index % 1440),
    lastTurnEnd: at((index + 7) % 1440),
  });
  const phases = LEGION_WORKFLOW_PHASES.filter((phase) => phase !== "done");
  const issues: Record<string, unknown> = {};
  for (let index = 0; index < 92; index += 1) {
    const key = `INFRA-${100 + index}`;
    const phase = phases[index % phases.length];
    issues[key] =
      index < running
        ? {
            key,
            generation: 2,
            phase,
            status: "in_progress",
            repo: "acme/widgets",
            defaultBranch: "main",
            ...(phase === "held" ? { holdReason: "escalated" } : {}),
            architect: claimView(index, "idle"),
            workers: Object.fromEntries(
              WORKER_ROLES.map((role, offset) => [
                role,
                {
                  claim: claimView(index * 6 + offset, offset === 0 ? "working" : "retired"),
                  pass: 1,
                  rounds: 0,
                },
              ])
            ),
            slot: { index: index % 8, admittedAt: at(index) },
            designGate: {
              artifactId: `artifact-${index}`,
              currentVersion: 3,
              approvedVersion: index % 2 === 0 ? 2 : null,
            },
            pullRequest: {
              repo: "acme/widgets",
              number: 1000 + index,
              head: `${index}`.padStart(40, "0"),
              state: "open",
              mergeState: "blocked",
              reviewDecision: "CHANGES_REQUESTED",
            },
          }
        : {
            key,
            generation: 2,
            phase: "done",
            status: "done",
            repo: "acme/widgets",
            defaultBranch: "main",
            workers: {},
          };
  }
  return legionState({
    issues,
    admission: {
      cap: 8,
      free: 0,
      active: ["INFRA-100", "INFRA-101"],
      waiting: ["INFRA-102"],
      blocked: [{ issue: "INFRA-102", blockedBy: ["INFRA-103", "INFRA-104"] }],
    },
    capabilities: [
      { name: "subagents", status: "present", detail: "6 agents discovered" },
      {
        name: "model-fallback",
        status: "open",
        detail: "no fallback configured",
        configLine: "capabilities.decided.model-fallback",
      },
    ],
  });
}

/** The controller's tool over `daemon`, recording each session its grant is minted for. */
function controllerTool(daemon: () => LegionDaemonClient, minted: string[]): RegisteredTool {
  return createLegionTool({
    pi,
    daemon,
    controllerGrant: async (sessionId) => {
      minted.push(sessionId);
      return { grantId: "controller-grant-1", expiresAt: "2099-01-01T00:00:00Z" };
    },
    onPhaseCompleted: () => {
      throw new Error("a controller completes no phase");
    },
    handoffCommit: noHandoffCommit,
    resolveDocument: noDocumentLookup,
    session: () => ({ kind: "controller", sessionID: "ses_controller" }),
  });
}

test("the controller reads the state without a grant and sets an issue's status with its own", async () => {
  const state = legionState({
    admission: { cap: 2, free: 0, active: ["LEGION-208"], waiting: [], blocked: [] },
    issues: {
      "LEGION-208": {
        key: "LEGION-208",
        generation: 1,
        phase: "held",
        status: "in_progress",
        holdReason: "escalated",
        workers: {},
      },
    },
  });
  const minted: string[] = [];
  const { calls, daemon } = recordingDaemon({ state });
  const tool = controllerTool(daemon, minted);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));

  // read_state with no field is the summary: the skill reads the issue's phase and hold, the
  // admission lists and the capability rows from it, and reads a full record with a filter.
  const summary = await run({ op: "read_state" });
  expect(summary.details).toEqual({});
  expect(summary.content).toEqual([
    {
      type: "text",
      text: expect.stringContaining("\nissues 1\nLEGION-208 held in_progress\n  hold escalated"),
    },
  ]);
  expect(calls).toEqual([["state", undefined]]);
  expect(minted).toEqual([]);

  await expect(run({ op: "set_status", issue: "LEGION-208", status: "backlog" })).resolves.toEqual({
    content: [{ type: "text", text: "{}" }],
    details: {},
  });
  expect(minted).toEqual(["ses_controller"]);
  expect(calls.at(-1)).toEqual([
    "issueStatus",
    { grantId: "controller-grant-1", issue: "LEGION-208", status: "backlog" },
  ]);

  // Refused before any mint: a status the board does not have, a missing issue, and every
  // operation of a claim.
  const before = calls.length;
  const refusals = [
    [{ op: "set_status", issue: "LEGION-208", status: "done" }, "todo, backlog, icebox"],
    [{ op: "set_status", status: "todo" }, "set_status requires issue"],
    [{ op: "read_state", to: "planning" }, 'read_state does not accept field "to"'],
    [{ op: "read_record", issue: "LEGION-208" }, "not available to a controller session"],
    [{ op: "handoff_complete", summary: "Done." }, "not available to a controller session"],
    [{ op: "sign_off", issue: "LEGION-208" }, "not available to a controller session"],
  ] as const;
  for (const [parameters, message] of refusals) {
    const result = await run(parameters);
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
  expect(calls).toHaveLength(before);
  expect(minted).toEqual(["ses_controller"]);
});

test("read_state with no field summarizes 92 issues under the tool's limit, naming every one", async () => {
  const state = stateOf92Issues();
  const { daemon } = recordingDaemon({ state });
  const tool = controllerTool(daemon, []);
  const result = await tool.execute(
    "",
    { op: "read_state" },
    undefined,
    undefined,
    context("ses_controller")
  );
  const [block] = result.content;
  const summary = block !== undefined && block.type === "text" ? block.text : "";

  // The INFRA deployment's own state at this size is about 160 KB of JSON, and the tool's output
  // reaches the controller cut at about 41 KB (dispatch://INFRA-329). This one is of that order,
  // and the summary of it is a fifth of the cut.
  expect(Buffer.byteLength(JSON.stringify(state), "utf8")).toBeGreaterThan(100 * 1024);
  expect(Buffer.byteLength(summary, "utf8")).toBeLessThan(30720);
  // Every recorded key is named, on its own line with its phase and status: a summary that drops
  // issues to fit is the bug this replaces.
  expect(summary).toContain("\nissues 92\n");
  const lines = summary.split("\n");
  for (const [key, issue] of Object.entries(state.issues)) {
    expect(lines).toContain(`${key} ${issue.phase} ${issue.status}`);
  }

  expect(summary).toContain("project INFRA boots 9 started 2026-10-30T01:02:03Z");
  expect(summary).toContain(
    "\nadmission cap 8 free 0\nactive INFRA-100 INFRA-101\nwaiting INFRA-102\nblocked INFRA-102 on INFRA-103 INFRA-104\n"
  );
  expect(summary).toContain("\ncapability subagents present\n");
  expect(summary).toContain(
    "\ncapability model-fallback open no fallback configured config capabilities.decided.model-fallback\n"
  );
  // An issue Legion still runs carries its slot, its gate and every claim's state with that
  // claim's last turn times; a hold carries its reason.
  expect(summary).toContain(
    [
      "\nINFRA-100 admitted in_progress",
      "  slot 0 since 2026-10-30T00:00:00Z",
      "  gate 3 approved 2",
      "  architect idle 2026-10-30T00:00:00Z 2026-10-30T00:07:00Z",
      "  planner working 2026-10-30T00:00:00Z 2026-10-30T00:07:00Z",
      "  implementer retired 2026-10-30T00:01:00Z 2026-10-30T00:08:00Z",
      "  tester retired 2026-10-30T00:02:00Z 2026-10-30T00:09:00Z",
      "  reviewer retired 2026-10-30T00:03:00Z 2026-10-30T00:10:00Z",
      "  merger retired 2026-10-30T00:04:00Z 2026-10-30T00:11:00Z\n",
    ].join("\n")
  );
  expect(summary).toContain("\n  hold escalated\n");
  expect(summary).toContain("  gate 3 approved none\n");
  // A done issue's claims are retired, so its line stands alone: that is what pays for naming
  // every key.
  expect(summary).toContain("\nINFRA-146 done done\nINFRA-147 done done\n");
});

test("read_state's filters answer full records, and name what they cannot select", async () => {
  const state = stateOf92Issues();
  const { daemon } = recordingDaemon({ state });
  const tool = controllerTool(daemon, []);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));

  // The record a filter answers is the state's own, every claim and count in it.
  await expect(run({ op: "read_state", issue: "INFRA-101" })).resolves.toEqual({
    content: [
      {
        type: "text",
        text: JSON.stringify({ issues: { "INFRA-101": state.issues["INFRA-101"] } }),
      },
    ],
    details: { issues: { "INFRA-101": state.issues["INFRA-101"] } },
  });
  await expect(
    run({ op: "read_state", issues: ["INFRA-101", "INFRA-130"] })
  ).resolves.toMatchObject({
    details: {
      issues: { "INFRA-101": state.issues["INFRA-101"], "INFRA-130": state.issues["INFRA-130"] },
    },
  });
  const reviewing = Object.fromEntries(
    Object.entries(state.issues).filter(([, issue]) => issue.phase === "reviewing")
  );
  expect(Object.keys(reviewing).length).toBeGreaterThan(1);
  await expect(run({ op: "read_state", phase: "reviewing" })).resolves.toMatchObject({
    details: { issues: reviewing },
  });
  await expect(run({ op: "read_state", settings: true })).resolves.toMatchObject({
    details: { settings: state.settings },
  });

  const refusals = [
    [{ op: "read_state", issue: "INFRA-999" }, "The daemon has no record for INFRA-999"],
    [
      { op: "read_state", issues: ["INFRA-998", "INFRA-999"] },
      "The daemon has no record for INFRA-998, INFRA-999",
    ],
    [
      { op: "read_state", issue: "INFRA-101", settings: true },
      "read_state takes one of issue, issues, phase, settings, not issue and settings",
    ],
    [{ op: "read_state", phase: "nowhere" }, `read_state's phase is one of ${LEGION_PHASES[0]}`],
    [{ op: "read_state", settings: false }, "read_state's settings is true"],
    [{ op: "read_state", issues: "INFRA-101" }, "read_state's issues is a list of issue keys"],
  ] as const;
  for (const [parameters, message] of refusals) {
    const result = await run(parameters);
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
});
