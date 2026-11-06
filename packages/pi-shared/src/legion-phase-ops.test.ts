import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LEGION_PHASE_BACKWARD_TARGETS } from "@legion/contracts/legion-api";
import { z } from "zod";
import { createLegionDaemonClient } from "./daemon-client";
import {
  type ClaimCapability,
  executePhaseOp,
  legionToolDescription,
  PHASE_OP_SENTENCES,
  PHASE_OPS,
  PhaseOpArguments,
  type PhaseOpRuntime,
  phaseOpParameters,
} from "./legion-phase-ops";

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

const implementer: ClaimCapability = {
  sessionID: "ses_implementer",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 5,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
};

/** What the implementer's round reports in the pane: its record and its pushed tip. */
const REPORT = {
  commit: "a".repeat(40),
  handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
  handoffVersion: 2,
};

/** A runtime over a stub daemon answering the golden fixtures, recording each route it is sent
 * and each body it is posted. `state` is what `GET /legion/v1/state` answers. */
function runtime(
  claim: ClaimCapability = implementer,
  state: Record<string, unknown> = daemonFixture("state.json")
) {
  const routes: string[] = [];
  const posted: Record<string, unknown>[] = [];
  const lookups: string[] = [];
  const answers: Record<string, Record<string, unknown>> = {
    "/legion/v1/state": state,
    "/legion/v1/grants": daemonFixture("grant.json"),
    "/legion/v1/handoff/complete": daemonFixture("handoff-complete.json"),
  };
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    routes.push(route);
    if (typeof init?.body === "string") posted.push(JSON.parse(init.body));
    return Response.json(answers[route] ?? {});
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    claim,
    handoffReport: async (phase, role) => {
      lookups.push(`${phase} ${role}`);
      return REPORT;
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  };
  return { ops, routes, posted, lookups };
}

test("the phase ops' fields, as a host declares them, are every field of every op, each optional, from the host's Zod", () => {
  const declared = z.toJSONSchema(z.object({ op: z.enum(PHASE_OPS), ...phaseOpParameters(z) }));
  expect(declared).toMatchObject({
    properties: {
      op: { enum: [...PHASE_OPS] },
      issue: { type: "string" },
      since: { type: "string" },
      summary: { type: "string" },
      verdict: { enum: ["pass", "pending", "fail"] },
      ready: { type: "boolean" },
      wake_at: { type: "string" },
      to: { enum: [...LEGION_PHASE_BACKWARD_TARGETS] },
      reason: { type: "string" },
      round: { type: "string" },
    },
    required: ["op"],
  });
  const fields = new Set(
    PhaseOpArguments.options
      .flatMap((option) => Object.keys(option.shape))
      .filter((f) => f !== "op")
  );
  expect(Object.keys(phaseOpParameters(z)).sort()).toEqual([...fields].sort());
});

test("the description sets the host's sentences before and after handoff_complete's contract, and ends on the host's Envoy publish tool", () => {
  const bare = legionToolDescription({ envoyPublish: "envoy_publish" });
  const placed = legionToolDescription({
    envoyPublish: "mcp__legion__envoy_publish",
    before: PHASE_OP_SENTENCES.readRecord,
    after: `${PHASE_OP_SENTENCES.requestBackwardMove}The controller's read_state returns the daemon's whole state. `,
  });
  expect(bare).not.toContain("read_record");
  expect(bare).not.toContain("request_backward_move");
  expect(bare.endsWith("role topic with envoy_publish.")).toBe(true);
  expect(placed).toContain(
    "this tool cannot spawn workers. read_record returns the record of `issue` from the daemon's state. handoff_complete (phase workers"
  );
  // Every host states `ready_for_update`, so its sentences are the template's own, between
  // handoff_complete's contract and the host's `after`.
  expect(placed).toContain(
    "arm nothing. ready_for_update answers the daemon's update task on this issue's pod: `round` is the round id that task names, and the daemon takes your answer for that round alone. Push every commit you want kept to the issue branch first, because the pod and its disk are replaced after every worker of the pod has answered. The call ends no phase. The daemon holds your next task until your process comes back on the new pod, in this same session. request_backward_move returns an issue to an earlier phase (`to`, `reason`): a phase worker moves its own issue; the tree's architect moves an issue in accepting, named by `issue`, back to implementing or testing, which counts a review round. The controller's read_state returns the daemon's whole state. What a later phase needs"
  );
  expect(placed.endsWith("role topic with mcp__legion__envoy_publish.")).toBe(true);
});

test("each op refuses its arguments whole before any daemon request, naming every problem", async () => {
  const { ops, routes } = runtime();
  const refusals = [
    [{ op: "handoff_complete" }, "handoff_complete requires summary"],
    [{ op: "handoff_complete", summary: "  " }, "handoff_complete requires summary"],
    [
      { op: "handoff_complete", summary: "x", verdict: "maybe" },
      "handoff_complete's verdict is pass, pending or fail",
    ],
    [
      { op: "handoff_complete", summary: "x", ready: "yes" },
      "handoff_complete's ready is true or false",
    ],
    [
      { op: "handoff_complete", summary: "x", verdict: "pending", wake_at: " " },
      "handoff_complete requires wake_at",
    ],
    [
      { op: "handoff_complete", summary: "x", verdict: "pending", wake_at: "tomorrow" },
      "handoff_complete's wake_at is an RFC 3339 time, as 2026-11-02T09:00:00Z is",
    ],
    [
      // Minute precision: Go's `time.Time` decode refuses it, so this tool refuses it too, by the
      // field's name rather than as the daemon's generic invalid-body answer (LegionInstant).
      { op: "handoff_complete", summary: "x", verdict: "pending", wake_at: "2026-11-06T10:00Z" },
      "handoff_complete's wake_at is an RFC 3339 time, as 2026-11-02T09:00:00Z is",
    ],
    [
      { op: "handoff_complete", summary: "x", phase: "plan", other: 1 },
      'handoff_complete does not accept field "phase"; handoff_complete does not accept field "other"',
    ],
    [{ op: "read_record" }, "read_record requires issue"],
    [{ op: "read_release_records" }, "read_release_records requires since"],
    [{ op: "read_release_records", since: " " }, "read_release_records requires since"],
    [
      { op: "read_release_records", since: "yesterday" },
      "read_release_records' since is an RFC 3339 time, as 2026-11-02T09:00:00Z is",
    ],
    [
      { op: "read_release_records", since: "2026-11-01T12:00:00Z", issue: "LEGION-208" },
      'read_release_records does not accept field "issue"',
    ],
    [{ op: "request_backward_move", reason: "r" }, "request_backward_move requires to"],
    [{ op: "request_backward_move", to: " ", reason: "r" }, "request_backward_move requires to"],
    [{ op: "request_backward_move", to: 44, reason: "r" }, "request_backward_move requires to"],
    [
      { op: "request_backward_move" },
      "request_backward_move requires to; request_backward_move requires reason",
    ],
    [
      { op: "request_backward_move", to: "integrating", reason: "r" },
      `request_backward_move's to is one of ${LEGION_PHASE_BACKWARD_TARGETS.join(", ")}`,
    ],
    [
      { op: "set_status" },
      "op is one of read_record, read_release_records, handoff_complete, request_backward_move",
    ],
    [{}, "op is one of read_record, read_release_records, handoff_complete, request_backward_move"],
  ] as const;
  for (const [input, message] of refusals) {
    await expect(executePhaseOp(ops, input)).rejects.toThrow(message);
  }
  expect(routes).toEqual([]);
});

test("handoff_complete reads the issue's phase, finds the record and commit for it and the claim's role, then mints its grant and posts them", async () => {
  const { ops, routes, posted, lookups } = runtime();
  await expect(
    executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", verdict: "pass" })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(lookups).toEqual(["implementing implementer"]);
  expect(routes).toEqual(["/legion/v1/state", "/legion/v1/grants", "/legion/v1/handoff/complete"]);
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Done.",
    verdict: "pass",
    ready: false,
    ...REPORT,
  });
});

/** `state.json` with a key no schema names at four depths: the top level, one issue, one worker's
 * claim — the path INFRA-1048's incident took, where one key of a daemon a contract ahead
 * failed every `legion` call — and the locator below that claim. */
function stateWithKeysNoSchemaNames(): Record<string, unknown> {
  const state = daemonFixture("state.json");
  const issues = state.issues as Record<string, Record<string, unknown>>;
  const issue = issues["LEGION-208"];
  const workers = issue.workers as Record<string, Record<string, unknown>>;
  const worker = workers.implementer;
  const claim = worker.claim as Record<string, unknown>;
  return {
    ...state,
    workerAdmission: { queue: ["LEGION-211"] },
    issues: {
      ...issues,
      "LEGION-208": {
        ...issue,
        escalation: { reason: "a field this build's contract has no name for" },
        workers: {
          ...workers,
          implementer: {
            ...worker,
            claim: {
              ...claim,
              lease: { holder: "daemon" },
              locator: { ...(claim.locator as Record<string, unknown>), zone: "us-east-1a" },
            },
          },
        },
      },
    },
  };
}

test("a state carrying keys no schema names at four depths is read, and read_record and handoff_complete hand none of them on", async () => {
  const golden = daemonFixture("state.json") as { issues: Record<string, unknown> };
  const { ops, routes } = runtime(implementer, stateWithKeysNoSchemaNames());

  // Awaited rather than `.resolves`, so a refusal of a key the reader must ignore is reported
  // with the path it named.
  expect(await executePhaseOp(ops, { op: "read_record", issue: "LEGION-208" })).toEqual({
    record: golden.issues["LEGION-208"],
  });
  expect(
    await executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", verdict: "pass" })
  ).toEqual(daemonFixture("handoff-complete.json"));
  expect(routes).toEqual([
    "/legion/v1/state",
    "/legion/v1/state",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
});

test("a root architect's handoff_complete is refused as unavailable before its own fields are checked, after any field it never takes", async () => {
  const root = runtime({ ...implementer, role: "architect" });
  const refusal =
    "handoff_complete is not available to a root architect session, which runs no phase";
  for (const input of [
    { op: "handoff_complete" },
    { op: "handoff_complete", summary: "x" },
    { op: "handoff_complete", summary: " ", verdict: "maybe", ready: "yes" },
  ]) {
    await expect(executePhaseOp(root.ops, input)).rejects.toThrow(new Error(refusal));
  }
  await expect(executePhaseOp(root.ops, { op: "handoff_complete", phase: "plan" })).rejects.toThrow(
    new Error(`handoff_complete does not accept field "phase"; ${refusal}`)
  );
  expect(root.routes).toEqual([]);
});

/** LEGION-208 closed by its merge, its production check open on its second look, as the daemon's
 * state shows it (`check` on the issue record): `role` is the role the check records as the one
 * that works it, and `last` false says a look follows this one. */
const openCheck = {
  repo: "acme/widgets",
  pullRequest: 1239,
  mergeCommit: "3e9c0a7b5d2f4e8a1c6b9d0f2a4e7c5b8d1f3a60",
  openedAt: "2026-09-22T08:40:12Z",
  role: "implementer",
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

/** The record of a closed issue whose check is open, `fields` over the check above. */
const checkRecord = (fields: Record<string, unknown> = {}) => ({
  check: { ...openCheck, ...fields },
});

/** The time a pending report names for its next look. */
const NEXT_LOOK = "2026-09-29T09:00:00Z";

/** A runtime whose daemon shows LEGION-208 in `done` with `record` on it, recording each request
 * body posted. `refusal` is what the daemon answers the completion with, success when none. */
function closedRuntime(
  claim: ClaimCapability,
  record: Record<string, unknown>,
  refusal?: { readonly status: number; readonly code: string; readonly error: string }
) {
  const posted: Record<string, unknown>[] = [];
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    if (route === "/legion/v1/state") {
      const state = daemonFixture("state.json") as { issues: Record<string, object> };
      return Response.json({
        ...state,
        issues: { "LEGION-208": { ...state.issues["LEGION-208"], phase: "done", ...record } },
      });
    }
    if (typeof init?.body === "string") posted.push(JSON.parse(init.body));
    if (route === "/legion/v1/grants") return Response.json(daemonFixture("grant.json"));
    if (route === "/legion/v1/handoff/complete" && refusal !== undefined) {
      return Response.json(
        { code: refusal.code, error: refusal.error },
        { status: refusal.status }
      );
    }
    return Response.json({});
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    claim,
    handoffReport: async (phase, role) => {
      throw new Error(`no handoff report expected for the ${role} at ${phase}`);
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  };
  return { ops, posted };
}

test("a report on the production check the claim's own role works carries a verdict and no commit, and a pending its next look", async () => {
  const { ops, posted } = closedRuntime(implementer, checkRecord());
  // Each of the three verdicts is posted with a fresh grant, the summary and no commit: no
  // handoff was written and nothing pushed, so the lookup is never asked. The pending names the
  // next look, which travels as the daemon's own `wakeAt`.
  for (const verdict of ["pending", "pass", "fail"]) {
    const wake = verdict === "pending" ? { wake_at: NEXT_LOOK } : {};
    await expect(
      executePhaseOp(ops, {
        op: "handoff_complete",
        summary: `Look 2: ${verdict}.`,
        verdict,
        ...wake,
      })
    ).resolves.toEqual({});
    expect(posted.at(-1)).toEqual({
      grantId: "grant-for-one-command",
      summary: `Look 2: ${verdict}.`,
      verdict,
      ready: false,
      ...(verdict === "pending" ? { wakeAt: NEXT_LOOK } : {}),
    });
  }
  expect(posted).toHaveLength(6);
});

test("a tester's pending report on its closed issue's check reaches the daemon, carrying the wake it named", async () => {
  const { ops, posted } = closedRuntime(
    { ...implementer, role: "tester" },
    checkRecord({ role: "tester" })
  );
  const summary = "Look 2: production does not carry the merge yet.";
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary,
      verdict: "pending",
      wake_at: NEXT_LOOK,
    })
  ).resolves.toEqual({});
  expect(posted).toEqual([
    { sessionId: "ses_implementer", secret: "U2VjcmV0", tree: "LEGION-208", issue: "LEGION-208" },
    {
      grantId: "grant-for-one-command",
      summary,
      verdict: "pending",
      ready: false,
      wakeAt: NEXT_LOOK,
    },
  ]);
});

test("a tester's completion on a done issue with no check object reaches the daemon, which answers CHECK_NOT_OPEN", async () => {
  // The state carries the check object only while the check is open, so a report on a check that
  // closed or never opened finds none: the plugin refuses nothing — not even the missing verdict
  // a check report needs — and posts the completion, which the daemon alone refuses.
  const { ops, posted } = closedRuntime(
    { ...implementer, role: "tester" },
    {},
    {
      status: 409,
      code: "CHECK_NOT_OPEN",
      error: "LEGION-208 has no open production check; this completion changed nothing",
    }
  );
  for (const input of [
    { op: "handoff_complete", summary: "Looked.", verdict: "pass" },
    { op: "handoff_complete", summary: "Looked." },
  ]) {
    await expect(executePhaseOp(ops, input)).rejects.toThrow(
      new Error(
        "POST /legion/v1/handoff/complete failed with 409 CHECK_NOT_OPEN: LEGION-208 has no open production check; this completion changed nothing"
      )
    );
  }
  // Each call posted a grant of its own and then the completion, the verdict as it was given.
  expect(posted.filter((body) => "grantId" in body)).toEqual([
    { grantId: "grant-for-one-command", summary: "Looked.", verdict: "pass", ready: false },
    { grantId: "grant-for-one-command", summary: "Looked.", verdict: "", ready: false },
  ]);
});

test("a report by a role the check does not record is refused before any call, nothing minted or posted", async () => {
  const { ops, posted } = closedRuntime({ ...implementer, role: "tester" }, checkRecord());
  await expect(
    executePhaseOp(ops, { op: "handoff_complete", summary: "Looked.", verdict: "pass" })
  ).rejects.toThrow(
    new Error(
      "LEGION-208's production check is the implementer's, and this session is the tester's: only the role the check records reports on it"
    )
  );
  expect(posted).toEqual([]);
});

test("each wrong shape of a check report is refused before any call, nothing minted or posted", async () => {
  const check = closedRuntime(implementer, checkRecord());
  const lastLook = closedRuntime(implementer, checkRecord({ last: true }));
  for (const [ops, input, message] of [
    [
      check.ops,
      { op: "handoff_complete", summary: "Looked." },
      "handoff_complete on a production check names a verdict of pass, pending or fail",
    ],
    [
      check.ops,
      { op: "handoff_complete", summary: "Looked.", verdict: "pass", ready: true },
      "handoff_complete on a production check is no READY: leave ready out",
    ],
    [
      check.ops,
      { op: "handoff_complete", summary: "Looked.", verdict: "pending" },
      "handoff_complete's pending report names wake_at, when the next look of LEGION-208's production check is due",
    ],
    [
      check.ops,
      { op: "handoff_complete", summary: "Looked.", verdict: "pass", wake_at: NEXT_LOOK },
      "handoff_complete names wake_at and no look follows it: a pass report closes the check; send it again without a time",
    ],
    [
      lastLook.ops,
      { op: "handoff_complete", summary: "Looked.", verdict: "pending", wake_at: NEXT_LOOK },
      "handoff_complete names wake_at and no look follows it: look 2 is the check's last, after which the check closes as not run; send it again without a time",
    ],
    // A time the route could not read is refused by the arguments themselves, before the record
    // is even read.
    [
      check.ops,
      { op: "handoff_complete", summary: "Looked.", verdict: "pending", wake_at: "tomorrow" },
      "handoff_complete's wake_at is an RFC 3339 time, as 2026-11-02T09:00:00Z is",
    ],
  ] as const) {
    await expect(executePhaseOp(ops, input)).rejects.toThrow(new Error(message));
  }
  expect(check.posted).toEqual([]);
  expect(lastLook.posted).toEqual([]);
});

test("the check's last look asks for no wake: a pending there is posted without one", async () => {
  const { ops, posted } = closedRuntime(implementer, checkRecord({ last: true }));
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Look 2: production never carried the merge.",
      verdict: "pending",
    })
  ).resolves.toEqual({});
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Look 2: production never carried the merge.",
    verdict: "pending",
    ready: false,
  });
});

test("pending is a production check's verdict: a phase completion naming it is refused once the record says so, nothing minted or posted", async () => {
  const { ops, routes, lookups } = runtime({ ...implementer, role: "tester" });
  await expect(
    executePhaseOp(ops, { op: "handoff_complete", summary: "Waiting.", verdict: "pending" })
  ).rejects.toThrow(
    new Error(
      "handoff_complete's verdict is pass or fail on a phase completion; pending is a production check's"
    )
  );
  expect(routes).toEqual(["/legion/v1/state"]);
  expect(lookups).toEqual([]);
});

test("wake_at is a check report's: a phase completion carrying one is refused, since the daemon reads none there", async () => {
  const { ops, routes, lookups } = runtime({ ...implementer, role: "tester" });
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Green.",
      verdict: "pass",
      wake_at: NEXT_LOOK,
    })
  ).rejects.toThrow(
    new Error(
      "handoff_complete's wake_at is a production check's pending report alone; a phase completion carries none"
    )
  );
  expect(routes).toEqual(["/legion/v1/state"]);
  expect(lookups).toEqual([]);
});

test("request_backward_move names the issue an architect returns from accepting, and only a phase worker's own issue", async () => {
  const posted: Record<string, unknown>[] = [];
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    if (typeof init?.body === "string") posted.push(JSON.parse(init.body));
    if (route === "/legion/v1/grants") return Response.json(daemonFixture("grant.json"));
    return Response.json({});
  }) as typeof fetch;
  const runtimeFor = (claim: ClaimCapability): PhaseOpRuntime => ({
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    claim,
    handoffReport: async () => REPORT,
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  });
  // The tree's architect (its grant serves the whole tree) names the child it sends back; a
  // sub-architect's role is architect too.
  for (const architect of [
    { ...implementer, role: "architect" as const },
    { ...implementer, role: "architect" as const, issue: "LEGION-209" },
  ]) {
    await expect(
      executePhaseOp(runtimeFor(architect), {
        op: "request_backward_move",
        to: "testing",
        reason: "the proof is thin",
        issue: "LEGION-210",
      })
    ).resolves.toEqual({});
    expect(posted.at(-1)).toEqual({
      grantId: "grant-for-one-command",
      to: "testing",
      reason: "the proof is thin",
      issue: "LEGION-210",
    });
    await expect(
      executePhaseOp(runtimeFor(architect), {
        op: "request_backward_move",
        to: "testing",
        reason: "the proof is thin",
      })
    ).rejects.toThrow(new Error("request_backward_move requires issue"));
  }
  // A phase worker leaves the issue out, or names its own; another issue is refused before any
  // request, as the daemon would refuse it (ISSUE_NOT_GRANTS).
  const before = posted.length;
  await expect(
    executePhaseOp(runtimeFor(implementer), {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: "LEGION-208",
    })
  ).resolves.toEqual({});
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    to: "implementing",
    reason: "test failed",
  });
  await expect(
    executePhaseOp(runtimeFor(implementer), {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: "LEGION-209",
    })
  ).rejects.toThrow(
    new Error("a phase worker moves only its own issue LEGION-208, not LEGION-209")
  );
  expect(posted).toHaveLength(before + 2);
});

test("read_record answers the issue's record, or names an issue the daemon has none for; request_backward_move posts with a fresh grant", async () => {
  const { ops, routes } = runtime();
  const state = daemonFixture("state.json") as { issues: Record<string, unknown> };
  await expect(executePhaseOp(ops, { op: "read_record", issue: "LEGION-208" })).resolves.toEqual({
    record: state.issues["LEGION-208"],
  });
  await expect(executePhaseOp(ops, { op: "read_record", issue: "LEGION-999" })).rejects.toThrow(
    "The daemon has no record for LEGION-999"
  );
  await expect(
    executePhaseOp(ops, {
      op: "request_backward_move",
      to: "planning",
      reason: "The plan misses X.",
    })
  ).resolves.toEqual({});
  expect(routes.slice(2)).toEqual(["/legion/v1/grants", "/legion/v1/phase/backward"]);
});

/** A window the daemon served: three releases — one woken five minutes later, one woken three
 * hours later and one still released — three resumes of them, the last with no first turn, and
 * two holds of two reasons. */
const RELEASE_RECORDS = {
  releases: [
    {
      id: 1,
      issue: "LEGION-208",
      generation: 2,
      roles: ["implementer"],
      at: "2026-11-01T12:00:00Z",
      bundleBytes: 1024,
    },
    {
      id: 2,
      issue: "LEGION-209",
      generation: 1,
      roles: ["implementer", "tester"],
      at: "2026-11-01T13:00:00Z",
      bundleBytes: 0,
    },
    {
      id: 3,
      issue: "LEGION-210",
      generation: 1,
      roles: ["planner"],
      at: "2026-11-01T17:00:00Z",
      bundleBytes: 512,
    },
  ],
  resumes: [
    {
      id: 1,
      release: 1,
      issue: "LEGION-208",
      role: "implementer",
      kind: "task",
      at: "2026-11-01T12:05:00Z",
      podReadyAt: "2026-11-01T12:06:00Z",
      firstTurnAt: "2026-11-01T12:07:00Z",
      stuck: false,
    },
    {
      id: 2,
      release: 2,
      issue: "LEGION-209",
      role: "implementer",
      kind: "message",
      at: "2026-11-01T16:00:00Z",
      podReadyAt: "2026-11-01T16:02:00Z",
      firstTurnAt: "2026-11-01T16:06:00Z",
      stuck: false,
    },
    {
      id: 3,
      release: 2,
      issue: "LEGION-209",
      role: "tester",
      kind: "message",
      at: "2026-11-01T16:00:30Z",
      podReadyAt: "2026-11-01T16:02:00Z",
      stuck: true,
    },
  ],
  holds: [
    {
      id: 1,
      issue: "LEGION-211",
      reason: "background_work",
      detail: "async-job",
      firstAt: "2026-11-01T12:50:00Z",
      at: "2026-11-01T13:00:00Z",
      passes: 11,
    },
    {
      id: 2,
      issue: "LEGION-212",
      reason: "turn_in_progress",
      firstAt: "2026-11-01T13:10:00Z",
      at: "2026-11-01T13:10:00Z",
      passes: 1,
    },
  ],
};

/** A runtime whose daemon answers `records` on the release-records route and nothing else,
 * recording what was posted, with the clock the open release is measured against. */
function recordsRuntime(records: unknown) {
  const posted: { readonly path: string; readonly body: unknown }[] = [];
  const stub = (async (input, init) => {
    posted.push({
      path: new URL(input.toString()).pathname,
      body: init?.body == null ? undefined : JSON.parse(init.body.toString()),
    });
    return Response.json(records);
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    claim: implementer,
    handoffReport: async () => {
      throw new Error("no handoff report expected");
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
    now: () => new Date("2026-11-01T18:00:00Z"),
  };
  return { ops, posted };
}

test("read_release_records posts the claim's own token and secret, and counts the window for a report", async () => {
  const { ops, posted } = recordsRuntime(RELEASE_RECORDS);

  await expect(
    executePhaseOp(ops, { op: "read_release_records", since: "2026-11-01T00:00:00Z" })
  ).resolves.toEqual({
    since: "2026-11-01T00:00:00Z",
    asOf: "2026-11-01T18:00:00.000Z",
    ...RELEASE_RECORDS,
    counts: {
      releases: 3,
      resumes: 3,
      resumesByKind: { task: 1, message: 2 },
      holds: 2,
      holdsByReason: { background_work: 1, turn_in_progress: 1 },
    },
    // Five minutes, three hours, and the hour the third release has been open at `now`.
    podHoursReleased: 4.08,
    wokenWithinTenMinutes: { releases: 1, share: 0.333 },
    // 120 and 360 seconds; the third resume has no first turn, so it is counted, not measured.
    wakeToFirstTurnSeconds: {
      measured: 2,
      withoutFirstTurn: 1,
      median: 120,
      p90: 360,
      max: 360,
    },
  });
  expect(posted).toEqual([
    {
      path: "/legion/v1/claims/release-records",
      body: {
        claimToken: "legion-legion-legion-208-implementer",
        secret: "U2VjcmV0",
        since: "2026-11-01T00:00:00Z",
      },
    },
  ]);
});

test("a window with nothing in it counts as nothing, and measures no wake", async () => {
  const { ops } = recordsRuntime({ releases: [], resumes: [], holds: [] });

  await expect(
    executePhaseOp(ops, { op: "read_release_records", since: "2026-11-01T00:00:00Z" })
  ).resolves.toEqual({
    since: "2026-11-01T00:00:00Z",
    asOf: "2026-11-01T18:00:00.000Z",
    releases: [],
    resumes: [],
    holds: [],
    counts: { releases: 0, resumes: 0, resumesByKind: {}, holds: 0, holdsByReason: {} },
    podHoursReleased: 0,
    wokenWithinTenMinutes: { releases: 0, share: 0 },
    wakeToFirstTurnSeconds: {
      measured: 0,
      withoutFirstTurn: 0,
      median: null,
      p90: null,
      max: null,
    },
  });
});

test("ready_for_update answers for the round its task named, with the branch head, and leaves the head out when the branch is not on the remote", async () => {
  const posted: { route: string; body: unknown }[] = [];
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    posted.push({
      route,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const head = "9f1c0a7d4b2e6f8a3c5d1e7b9a0f2c4d6e8b1a35";
  const round = "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22";
  const runtimeFor = (branchHead: () => Promise<string>): PhaseOpRuntime => ({
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    claim: implementer,
    handoffReport: async () => {
      throw new Error("no handoff report expected");
    },
    branchHead,
  });
  // The claim authenticates the signal, so no grant is minted: the one request is the signal.
  // The round is the agent's own word, read from the update task it answers, and it travels as
  // it was given.
  await expect(
    executePhaseOp(
      runtimeFor(async () => head),
      { op: "ready_for_update", round }
    )
  ).resolves.toEqual({});
  expect(posted).toEqual([
    {
      route: "/legion/v1/claims/ready-for-update",
      body: {
        claimToken: "legion-legion-legion-208-implementer",
        sessionId: "ses_implementer",
        secret: "U2VjcmV0",
        generation: 5,
        round,
        head,
      },
    },
  ]);

  // A branch no push has created yet (a planner's) carries no head at all.
  await expect(
    executePhaseOp(
      runtimeFor(async () => ""),
      { op: "ready_for_update", round }
    )
  ).resolves.toEqual({});
  expect(posted.at(-1)?.body).toEqual({
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: "ses_implementer",
    secret: "U2VjcmV0",
    generation: 5,
    round,
  });

  // The round is the one field the agent passes: the head is the plugin's to resolve.
  await expect(
    executePhaseOp(
      runtimeFor(async () => head),
      { op: "ready_for_update", round, head }
    )
  ).rejects.toThrow(new Error('ready_for_update does not accept field "head"'));
  expect(posted).toHaveLength(2);
});

// The answer counts for the round the worker read and for no other, so the daemon refuses one
// naming another round (`supervise.ErrForeignUpdateRound`). A call that names no round at all
// cannot be sent: the daemon would have nothing to hold the answer to, and the signal would be
// filed against whatever round the claim happens to stand in. It is refused here instead, before
// any daemon request.
test("ready_for_update with no round is refused before any daemon request", async () => {
  const posted: string[] = [];
  const stub = (async (input) => {
    posted.push(new URL(input.toString()).pathname);
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    claim: implementer,
    handoffReport: async () => {
      throw new Error("no handoff report expected");
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  };
  for (const input of [{ op: "ready_for_update" }, { op: "ready_for_update", round: "  " }]) {
    await expect(executePhaseOp(ops, input)).rejects.toThrow(
      new Error("ready_for_update requires round")
    );
  }
  expect(posted).toEqual([]);
});

test("ready_for_update signals without a head when the branch-head lookup fails, and says so", async () => {
  const posted: { route: string; body: unknown }[] = [];
  const stub = (async (input, init) => {
    posted.push({
      route: new URL(input.toString()).pathname,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const failure =
    'jj log -r remote_bookmarks(exact:"legion/LEGION-208", exact:"origin") failed: Error: The lockfile at \'/workspace/.jj/working_copy/working_copy.lock\' might need manual deletion';
  // A workspace whose jj cannot answer still signals: the round waits for every claim of the pod,
  // and the daemon verifies no head. The answer carries the lookup's own message.
  await expect(
    executePhaseOp(
      {
        daemon: createLegionDaemonClient("http://daemon.test", stub),
        claim: implementer,
        handoffReport: async () => {
          throw new Error("no handoff report expected");
        },
        branchHead: async () => {
          throw new Error(failure);
        },
      },
      { op: "ready_for_update", round: "plugin:44" }
    )
  ).resolves.toEqual({
    note:
      "The daemon has your ready_for_update signal. The signal reports no branch head, because " +
      "the branch-head lookup failed. The daemon checks no head, so your signal counts without " +
      `one. Repair what the lookup's message names before your next jj command. The lookup said: ${failure}`,
  });
  expect(posted).toEqual([
    {
      route: "/legion/v1/claims/ready-for-update",
      body: {
        claimToken: "legion-legion-legion-208-implementer",
        sessionId: "ses_implementer",
        secret: "U2VjcmV0",
        generation: 5,
        round: "plugin:44",
      },
    },
  ]);
});
