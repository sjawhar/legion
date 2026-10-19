import { expect, test } from "bun:test";
import type { LegionRole } from "@legion/contracts";
import type { LegionPhase } from "@legion/contracts/legion-api";
import { findHandoffCommit, type JjRunner } from "@legion/pi-shared/handoff-commit";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
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
  "handoff_complete (phase workers and sub-architects; the daemon accepts a completion only from the role working the issue's current phase) " +
  "reports this phase complete: `summary` (two sentences for the architect, or the merger's READY packet), `verdict` pass|fail from the tester alone " +
  "(the daemon refuses a verdict from any other role), `ready: true` for the merger's READY. A handoff is a committed file, `.legion/<issue>/<phase>.json`: the pushed commit " +
  "carrying it is found here, with the pane's jj, and reported, so write it, commit it and push the branch (`jj git push`) before calling; this call " +
  "refuses, posting nothing, a handoff still in the working copy, missing, not committed on this branch, carried by another pane's commit, or not yet " +
  "on `legion/<issue>@origin`, each naming the remedy. The daemon refuses HANDOFF_NOT_NEW (the commit is the one you reported for your previous " +
  "phase: write and commit this phase's handoff) and, for READY, each refusal the merger's role names; a refused completion changed " +
  "nothing, so fix what it names and complete again. An accepted READY whose `note` begins MANUAL_MERGE_REQUIRED: is one a person merges: arm nothing. " +
  "The controller's read_state returns the daemon's whole state and set_status moves an issue to todo, backlog or icebox. " +
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
        "retro",
        "merging",
        "awaiting_merge",
        "production_check",
        "done",
        "held",
      ],
    },
    reason: { type: "string" },
    decision: { type: "string", enum: ["retry", "escalate"] },
    summary: { type: "string" },
    verdict: { type: "string", enum: ["pass", "fail"] },
    ready: { type: "boolean" },
    status: { type: "string", enum: ["todo", "backlog", "icebox"] },
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

  await expect(
    run(worker, { op: "request_backward_move", to: "implementing", reason: "test failed" })
  ).resolves.toMatchObject({ details: {} });

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
      "POST /legion/v1/signoff failed with 409 SIGNOFF_EARLY: production check is incomplete"
    ),
    { code: "SIGNOFF_EARLY", detail: "production check is incomplete" }
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
    content: [{ type: "text", text: expect.stringContaining("SIGNOFF_EARLY") }],
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
        note: "no check is required on main, so READY was published without reading the head's checks",
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
        text: '{"note":"no check is required on main, so READY was published without reading the head\'s checks"}',
      },
    ],
    details: {
      note: "no check is required on main, so READY was published without reading the head's checks",
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
    [claimSession("tester"), { summary: "x", verdict: "maybe" }, "verdict is pass or fail"],
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

test("the controller reads the whole state without a grant and sets an issue's status with its own", async () => {
  const state = {
    daemon: { project: "OMP" },
    admission: { cap: 2, active: ["LEGION-208"], waiting: [] },
    issues: { "LEGION-208": { key: "LEGION-208", phase: "held", holdReason: "escalated" } },
    capabilities: [],
  };
  const minted: string[] = [];
  const { calls, daemon } = recordingDaemon({ state });
  const tool = createLegionTool({
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
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));

  // read_state is the state verbatim: the skill reads `issues.<KEY>.phase|holdReason`,
  // `admission` and `capabilities` from it.
  await expect(run({ op: "read_state" })).resolves.toEqual({
    content: [{ type: "text", text: JSON.stringify(state) }],
    details: state,
  });
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
    [{ op: "read_state", issue: "LEGION-208" }, 'read_state does not accept field "issue"'],
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
