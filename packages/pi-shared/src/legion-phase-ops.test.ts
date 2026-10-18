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

/** A runtime over a stub daemon answering the golden fixtures, recording each route it is sent. */
function runtime(claim: ClaimCapability = implementer, commit = "a".repeat(40)) {
  const routes: string[] = [];
  const lookups: string[] = [];
  const answers: Record<string, Record<string, unknown>> = {
    "/legion/v1/state": daemonFixture("state.json"),
    "/legion/v1/grants": daemonFixture("grant.json"),
    "/legion/v1/handoff/complete": daemonFixture("handoff-complete.json"),
  };
  const stub = (async (input) => {
    const route = new URL(input.toString()).pathname;
    routes.push(route);
    return Response.json(answers[route] ?? {});
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    claim,
    handoffCommit: async (phase, role) => {
      lookups.push(`${phase} ${role}`);
      return commit;
    },
  };
  return { ops, routes, lookups };
}

test("the phase ops' fields, as a host declares them, are every field of every op, each optional, from the host's Zod", () => {
  const declared = z.toJSONSchema(z.object({ op: z.enum(PHASE_OPS), ...phaseOpParameters(z) }));
  expect(declared).toMatchObject({
    properties: {
      op: { enum: [...PHASE_OPS] },
      issue: { type: "string" },
      summary: { type: "string" },
      verdict: { enum: ["pass", "fail"] },
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
    "arm nothing. request_backward_move asks the daemon to move this issue back to an earlier phase, `to`, for `reason`. The controller's read_state returns the daemon's whole state. What a later phase needs"
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
      "handoff_complete's verdict is pass or fail",
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

test("handoff_complete reads the issue's phase, finds the commit for it and the claim's role, then mints its grant and posts", async () => {
  const { ops, routes, lookups } = runtime();
  await expect(
    executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", verdict: "pass" })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(lookups).toEqual(["implementing implementer"]);
  expect(routes).toEqual(["/legion/v1/state", "/legion/v1/grants", "/legion/v1/handoff/complete"]);
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
