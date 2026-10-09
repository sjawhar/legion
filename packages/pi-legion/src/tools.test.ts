import { expect, test } from "bun:test";
import type { LegionRole } from "@legion/contracts";
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

/** A claim's session on LEGION-208: a root architect's, a sub-architect's (its issue is not its
 * tree), or a phase worker's by role. */
function claimSession(role: LegionRole, issue = "LEGION-208"): LegionToolSession {
  return {
    kind: role === "architect" ? "architect" : "phase-worker",
    role,
    sessionId: "ses_208",
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
    "threadsResolve",
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

test("handoff_complete posts the completion with a fresh grant and no commit, closes the phase on success, and answers the daemon's note", async () => {
  const completed: string[] = [];
  const { calls, daemon } = recordingDaemon({
    answers: {
      handoffComplete: {
        note: "no check is required on main, so READY was published without reading the head's checks",
      },
    },
  });
  const run = (session: LegionToolSession, parameters: Record<string, unknown>) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
      resolveDocument: noDocumentLookup,
      session: () => session,
    }).execute("", parameters, undefined, undefined, context());

  // The implementer reports no verdict and no READY; the request carries the wire's zero values
  // for both and nothing else: the daemon reads the issue branch's head on GitHub itself.
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
    [
      "grant",
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-208" },
    ],
    ["handoffComplete", { grantId: "grant-208", summary: "Done.", verdict: "", ready: false }],
  ]);
  expect(calls.at(-1)?.[1]).not.toHaveProperty("commit");
  expect(completed).toEqual(["ses_208"]);

  // The tester's verdict and the merger's READY travel as given; a sub-architect completes too.
  await run(claimSession("tester"), { op: "handoff_complete", summary: "Red.", verdict: "fail" });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    { grantId: "grant-208", summary: "Red.", verdict: "fail", ready: false },
  ]);
  await run(claimSession("merger"), { op: "handoff_complete", summary: "READY", ready: true });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    { grantId: "grant-208", summary: "READY", verdict: "", ready: true },
  ]);
  await run(claimSession("architect", "LEGION-209"), {
    op: "handoff_complete",
    summary: "Children released.",
  });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    { grantId: "grant-208", summary: "Children released.", verdict: "", ready: false },
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
});

test("a refused completion surfaces the daemon's code and leaves the phase open", async () => {
  const refusal = Object.assign(
    new Error(
      "POST /legion/v1/handoff/complete failed with 409 HANDOFF_FILE_MISSING: no .legion/LEGION-208/implement.json at head abc123 of legion/LEGION-208: write, commit and push this phase's handoff, then complete again"
    ),
    { code: "HANDOFF_FILE_MISSING" }
  );
  const completed: string[] = [];
  const { daemon } = recordingDaemon({ answers: { handoffComplete: refusal } });
  const tool = createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
    resolveDocument: noDocumentLookup,
    session: () => claimSession("implementer"),
  });

  await expect(
    tool.execute("", { op: "handoff_complete", summary: "Done." }, undefined, undefined, context())
  ).resolves.toMatchObject({
    isError: true,
    content: [{ type: "text", text: expect.stringContaining("HANDOFF_FILE_MISSING") }],
  });
  expect(completed).toEqual([]);
});

test("resolve_threads posts the named thread ids against the record's pull request, for the reviewer alone", async () => {
  const outcome = {
    threads: [
      { thread: "PRRT_a", resolved: true },
      { thread: "PRRT_b", resolved: true, reason: "already resolved" },
    ],
  };
  const withPullRequest = {
    issues: {
      "LEGION-208": { key: "LEGION-208", phase: "reviewing", pullRequest: { number: 42 } },
    },
  };
  const run = (
    session: LegionToolSession,
    parameters: Record<string, unknown>,
    state: unknown = withPullRequest
  ) => {
    const recorded = recordingDaemon({ state, answers: { threadsResolve: outcome } });
    return {
      calls: recorded.calls,
      result: createLegionTool({
        pi,
        daemon: recorded.daemon,
        controllerGrant: noControllerGrant,
        onPhaseCompleted: () => undefined,
        resolveDocument: noDocumentLookup,
        session: () => session,
      }).execute("", { op: "resolve_threads", ...parameters }, undefined, undefined, context()),
    };
  };

  const reviewer = run(claimSession("reviewer"), {
    repo: "acme/widgets",
    threads: ["PRRT_a", "PRRT_b"],
  });
  await expect(reviewer.result).resolves.toEqual({
    content: [{ type: "text", text: JSON.stringify(outcome) }],
    details: outcome,
  });
  expect(reviewer.calls).toEqual([
    ["state", undefined],
    [
      "grant",
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-208" },
    ],
    [
      "threadsResolve",
      { grantId: "grant-208", repo: "acme/widgets", number: 42, threads: ["PRRT_a", "PRRT_b"] },
    ],
  ]);

  // Refused before any daemon call: every other role (the implementer resolves its own threads
  // with gh), an empty or malformed list, no repository, and a sub-architect.
  const refusals = [
    [claimSession("implementer"), { repo: "acme/widgets", threads: ["PRRT_a"] }, "reviewer alone"],
    [claimSession("merger"), { repo: "acme/widgets", threads: ["PRRT_a"] }, "reviewer alone"],
    [
      claimSession("architect", "LEGION-209"),
      { repo: "acme/widgets", threads: ["PRRT_a"] },
      "not available to a architect session",
    ],
    [claimSession("reviewer"), { repo: "acme/widgets", threads: [] }, "at least one, none empty"],
    [
      claimSession("reviewer"),
      { repo: "acme/widgets", threads: [" "] },
      "at least one, none empty",
    ],
    [claimSession("reviewer"), { repo: "acme/widgets" }, "at least one, none empty"],
    [claimSession("reviewer"), { threads: ["PRRT_a"] }, "resolve_threads requires repo"],
  ] as const;
  for (const [session, parameters, message] of refusals) {
    const { calls, result } = run(session, parameters);
    const answer = await result;
    expect(answer.isError).toBe(true);
    expect(JSON.stringify(answer.content)).toContain(message);
    expect(calls).toEqual([]);
  }

  // An issue with no pull request recorded has no threads to resolve: refused after the state
  // read, before any grant is minted.
  const unopened = run(
    claimSession("reviewer"),
    { repo: "acme/widgets", threads: ["PRRT_a"] },
    { issues: { "LEGION-208": { key: "LEGION-208", phase: "reviewing" } } }
  );
  const answer = await unopened.result;
  expect(answer.isError).toBe(true);
  expect(JSON.stringify(answer.content)).toContain("LEGION-208 has no pull request recorded");
  expect(unopened.calls).toEqual([["state", undefined]]);
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
    resolveDocument: noDocumentLookup,
    session: () => ({ kind: "controller", sessionId: "ses_controller" }),
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
    [{ op: "resolve_threads", threads: ["PRRT_a"] }, "not available to a controller session"],
  ] as const;
  for (const [parameters, message] of refusals) {
    const result = await run(parameters);
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
  expect(calls).toHaveLength(before);
  expect(minted).toEqual(["ses_controller"]);
});
