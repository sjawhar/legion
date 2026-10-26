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
 * and each body it is posted. */
function runtime(claim: ClaimCapability = implementer) {
  const routes: string[] = [];
  const posted: Record<string, unknown>[] = [];
  const lookups: string[] = [];
  const answers: Record<string, Record<string, unknown>> = {
    "/legion/v1/state": daemonFixture("state.json"),
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
  };
  return { ops, routes, posted, lookups };
}

test("the phase ops' fields, as a host declares them, are every field of every op, each optional, from the host's Zod", () => {
  const declared = z.toJSONSchema(z.object({ op: z.enum(PHASE_OPS), ...phaseOpParameters(z) }));
  expect(declared).toMatchObject({
    properties: {
      op: { enum: [...PHASE_OPS] },
      issue: { type: "string" },
      summary: { type: "string" },
      verdict: { enum: ["pass", "pending", "fail"] },
      ready: { type: "boolean" },
      to: { enum: [...LEGION_PHASE_BACKWARD_TARGETS] },
      reason: { type: "string" },
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
  expect(placed).toContain(
    "arm nothing. request_backward_move returns an issue to an earlier phase (`to`, `reason`): a phase worker moves its own issue; the tree's architect moves an issue in accepting, named by `issue`, back to implementing or testing, which counts a review round. The controller's read_state returns the daemon's whole state. What a later phase needs"
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
      { op: "handoff_complete", summary: "x", phase: "plan", other: 1 },
      'handoff_complete does not accept field "phase"; handoff_complete does not accept field "other"',
    ],
    [{ op: "read_record" }, "read_record requires issue"],
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
    [{ op: "set_status" }, "op is one of read_record, handoff_complete, request_backward_move"],
    [{}, "op is one of read_record, handoff_complete, request_backward_move"],
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
 * state shows it (`check` on the issue record). */
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

/** A runtime whose daemon shows LEGION-208 in `done` with `record` on it, recording each request
 * body posted. */
function closedRuntime(claim: ClaimCapability, record: Record<string, unknown>) {
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
    return Response.json({});
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    claim,
    handoffReport: async (phase, role) => {
      throw new Error(`no handoff report expected for the ${role} at ${phase}`);
    },
  };
  return { ops, posted };
}

test("the implementer's report on its closed issue's production check carries a verdict and no commit", async () => {
  const { ops, posted } = closedRuntime(implementer, { check: openCheck });
  // Each of the three verdicts is posted with a fresh grant, the summary and no commit: no
  // handoff was written and nothing pushed, so the lookup is never asked.
  for (const verdict of ["pending", "pass", "fail"]) {
    await expect(
      executePhaseOp(ops, { op: "handoff_complete", summary: `Look 2: ${verdict}.`, verdict })
    ).resolves.toEqual({});
    expect(posted.at(-1)).toEqual({
      grantId: "grant-for-one-command",
      summary: `Look 2: ${verdict}.`,
      verdict,
      ready: false,
    });
  }
  expect(posted).toHaveLength(6);

  // Refused after the record is read, with nothing minted or posted: a report with no verdict,
  // and one sent as READY.
  for (const [input, message] of [
    [
      { op: "handoff_complete", summary: "Looked." },
      "handoff_complete on a production check names a verdict of pass, pending or fail",
    ],
    [
      { op: "handoff_complete", summary: "Looked.", verdict: "pass", ready: true },
      "handoff_complete on a production check is no READY: leave ready out",
    ],
  ] as const) {
    await expect(executePhaseOp(ops, input)).rejects.toThrow(new Error(message));
  }
  expect(posted).toHaveLength(6);

  // Only the implementer reports on the check: any other role's completion on the closed issue is
  // a phase completion, which looks its commit up (and the daemon then refuses it naming whose
  // phase it is).
  const tester = closedRuntime({ ...implementer, role: "tester" }, { check: openCheck });
  await expect(
    executePhaseOp(tester.ops, { op: "handoff_complete", summary: "Looked.", verdict: "pass" })
  ).rejects.toThrow(new Error("no handoff report expected for the tester at done"));
  expect(tester.posted).toEqual([]);

  // The implementer's completion on a done issue is a check report whatever the record shows, as
  // the daemon reads it (api/handoff.go): once the check closed, or when none opened, no commit is
  // looked up (a done issue has no handoff for it) and the report is posted with the verdict as
  // given, for the daemon's one refusal, CHECK_NOT_OPEN.
  const closed = closedRuntime(implementer, {});
  await expect(
    executePhaseOp(closed.ops, { op: "handoff_complete", summary: "Looked." })
  ).resolves.toEqual({});
  expect(closed.posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Looked.",
    verdict: "",
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
