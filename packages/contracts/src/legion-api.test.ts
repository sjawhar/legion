import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  LegionChildRequest,
  LegionControllerGrantRequest,
  LegionControllerRegisterResponse,
  LegionControllerSecretResponse,
  LegionEmptyResponse,
  LegionErrorResponse,
  LegionEscalateRequest,
  LegionGateRegisterRequest,
  LegionGrantRequest,
  LegionGrantResponse,
  LegionHandoffCompleteRequest,
  LegionHandoffCompleteResponse,
  LegionIssueStatusRequest,
  LegionOperatorClaimResponse,
  LegionOperatorClaimsResponse,
  LegionPhaseBackwardRequest,
  LegionPhaseRetryRequest,
  LegionReadyRequest,
  LegionRegisterResponse,
  LegionRootCloseRequest,
  LegionSignOffRequest,
  LegionStateResponse,
  LegionWaveReleaseRequest,
  LegionWaveReleaseResponse,
} from "./legion-api";

const fixtureDir = path.join(import.meta.dir, "..", "fixtures", "daemon-api");

/** Every fixture the Go golden test writes, with the schema that must accept it. A new fixture
 * with no entry here fails the first test rather than going unchecked. */
const schemas: Record<string, z.ZodType> = {
  "state.json": LegionStateResponse,
  "state-stage3.json": LegionStateResponse,
  "state-operator-claim.json": LegionStateResponse,
  // Written by internal/projection's own golden test: what the state route projects while the
  // daemon's own controller (`controller: daemon`) holds its claim on no issue.
  "state-controller-claim.json": LegionStateResponse,
  "register.json": LegionRegisterResponse,
  // The claim wire's `claim.ReadyRequest` with a capability report, the one request body a Go
  // golden test writes: what a session posts on `claims/ready` (contract 19).
  "ready.json": LegionReadyRequest,
  "register-controller.json": LegionControllerRegisterResponse,
  "controller-secret.json": LegionControllerSecretResponse,
  "error.json": LegionErrorResponse,
  "operator-claim.json": LegionOperatorClaimResponse,
  "operator-claims.json": LegionOperatorClaimsResponse,
  "grant.json": LegionGrantResponse,
  "handoff-complete.json": LegionHandoffCompleteResponse,
  "issue-status.json": LegionEmptyResponse,
  "gate-register.json": LegionEmptyResponse,
  "wave-release.json": LegionWaveReleaseResponse,
  "phase-backward.json": LegionEmptyResponse,
  "phase-retry.json": LegionEmptyResponse,
  "signoff.json": LegionEmptyResponse,
  "root-close.json": LegionEmptyResponse,
  "child-park.json": LegionEmptyResponse,
  "child-rerun.json": LegionEmptyResponse,
  // No route answers this one: it is the contract number the daemon's boot gate requires of the
  // installed plugin (`internal/api/version.go`), and the plugin's own test pins its manifest's
  // `legion.daemonApiVersion` to it.
  "version.json": z.strictObject({ daemonApiVersion: z.number().int().positive() }),
};

function fixture(name: string): unknown {
  return JSON.parse(readFileSync(path.join(fixtureDir, name), "utf8"));
}

test("every Go-written fixture parses through the strict schema", () => {
  const names = readdirSync(fixtureDir).filter((name) => name.endsWith(".json"));
  expect(names.length).toBeGreaterThan(0);

  for (const name of names) {
    const schema = schemas[name];
    expect(schema, `${name} has no schema in legion-api.test.ts`).toBeDefined();
    const parsed = schema?.safeParse(fixture(name));
    expect(parsed?.error?.issues ?? [], `${name} failed the schema`).toEqual([]);
  }
});

test("state accepts optional fields emitted by later workflow slices", () => {
  const current = fixture("state.json");
  expect(LegionStateResponse.safeParse(current).success).toBeTrue();

  const later = fixture("state.json") as {
    admission: Record<string, unknown>;
    issues: Record<string, { phase: string; slot?: Record<string, unknown> }>;
  };
  later.admission.free = 1;
  later.issues["LEGION-208"]!.phase = "integrating";
  later.issues["LEGION-208"]!.slot!.lentTo = "LEGION-209";

  expect(LegionStateResponse.safeParse(later).success).toBeTrue();
});

test("state rejects malformed optional workflow fields", () => {
  const negativeFree = fixture("state.json") as { admission: Record<string, unknown> };
  negativeFree.admission.free = -1;
  expect(LegionStateResponse.safeParse(negativeFree).success).toBeFalse();

  const malformedLender = fixture("state.json") as {
    issues: Record<string, { slot?: Record<string, unknown> }>;
  };
  malformedLender.issues["LEGION-208"]!.slot!.lentTo = "not an issue key";
  expect(LegionStateResponse.safeParse(malformedLender).success).toBeFalse();
});

test("state accepts the existing done phase", () => {
  const completed = fixture("state.json") as {
    issues: Record<string, { phase: string }>;
  };
  completed.issues["LEGION-208"]!.phase = "done";

  expect(LegionStateResponse.safeParse(completed).success).toBeTrue();
});

test("a backward move rejects future read-only phases", () => {
  expect(
    LegionPhaseBackwardRequest.safeParse({
      grantId: "grant-208",
      to: "integrating",
      reason: "test failed",
    }).success
  ).toBeFalse();
});

test("every workflow request has a strict schema", () => {
  const requests: ReadonlyArray<readonly [string, z.ZodType, Record<string, unknown>]> = [
    [
      "grant",
      LegionGrantRequest,
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-209" },
    ],
    [
      "controller grant",
      LegionControllerGrantRequest,
      { sessionId: "ses_controller", secret: "s" },
    ],
    [
      "handoff complete",
      LegionHandoffCompleteRequest,
      { grantId: "grant-208", summary: "completed", verdict: "", ready: false, commit: "abc123" },
    ],
    [
      "issue status",
      LegionIssueStatusRequest,
      { grantId: "grant-208", issue: "LEGION-208", status: "todo" },
    ],
    [
      "gate register",
      LegionGateRegisterRequest,
      {
        grantId: "grant-208",
        issue: "LEGION-208",
        artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
        version: 7,
      },
    ],
    ["wave release", LegionWaveReleaseRequest, { grantId: "grant-208", issues: ["LEGION-209"] }],
    [
      "phase backward",
      LegionPhaseBackwardRequest,
      { grantId: "grant-208", to: "implementing", reason: "test failed" },
    ],
    [
      "phase retry",
      LegionPhaseRetryRequest,
      { grantId: "grant-208", issue: "LEGION-208", decision: "retry" },
    ],
    [
      "escalate",
      LegionEscalateRequest,
      { grantId: "grant-208", issue: "LEGION-208", reason: "needs controller triage" },
    ],
    ["signoff", LegionSignOffRequest, { grantId: "grant-208", issue: "LEGION-208" }],
    [
      "root close",
      LegionRootCloseRequest,
      { grantId: "grant-208", issue: "LEGION-208", reason: "no change" },
    ],
    ["child", LegionChildRequest, { grantId: "grant-208", issue: "LEGION-209" }],
  ];

  for (const [name, schema, request] of requests) {
    expect(schema.safeParse(request).success, `${name} accepts its route request`).toBeTrue();
    expect(
      schema.safeParse({ ...request, unexpected: true }).success,
      `${name} refuses an added field`
    ).toBeFalse();
    const [required] = Object.keys(request);
    const missing = { ...request };
    delete missing[required ?? ""];
    expect(
      schema.safeParse(missing).success,
      `${name} refuses a missing required field`
    ).toBeFalse();
  }
});

test("an escalation rejects empty required fields", () => {
  const request = { grantId: "grant-208", issue: "LEGION-208", reason: "needs controller triage" };
  for (const field of ["grantId", "issue", "reason"] as const) {
    expect(LegionEscalateRequest.safeParse({ ...request, [field]: "" }).success).toBeFalse();
  }
});

test("a workflow refusal preserves its stable code and message", () => {
  expect(
    LegionErrorResponse.safeParse({
      code: "GATE_UNAPPROVED",
      error: "the current spec version needs approval",
    }).success
  ).toBeTrue();
});

test("a field the Go shape does not carry is refused", () => {
  const mutated = fixture("state.json") as Record<string, unknown>;
  mutated.workerAdmission = { queue: [] };

  expect(LegionStateResponse.safeParse(mutated).success).toBeFalse();
});

test("a field the Go shape requires cannot be dropped", () => {
  const mutated = fixture("state.json") as { admission: Record<string, unknown> };
  delete mutated.admission.waiting;

  expect(LegionStateResponse.safeParse(mutated).success).toBeFalse();
});

test("the Stage 3 state golden carries the record status and pending Dispatch status write", () => {
  const state = LegionStateResponse.parse(fixture("state-stage3.json"));

  expect(state.issues["LEGION-208"]?.status).toBe("needs_review");
  expect(state.pendingStatusWrites).toEqual([
    {
      issue: "LEGION-208",
      payload: { status: "needs_review" },
      attempts: 2,
      nextAt: "2026-09-22T17:32:00Z",
      lastError: "Dispatch unavailable",
    },
  ]);
});

test("a Stage 3 issue without its Dispatch status is refused", () => {
  const state = fixture("state-stage3.json") as {
    issues: Record<string, Record<string, unknown>>;
  };
  delete state.issues["LEGION-208"]?.status;

  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
});

// The state's capability report (contract 16): the golden carries a decided row with the operator's
// reason and an open row with the legion.yaml line that records a decision, beside the present
// (codegraph among them: the image carries the tooling and a pod's launch loads it with extension
// discovery on) and withheld rows, and the six live rows unchecked — no session has reported, so
// the deployment renders each as `unchecked` with its ruling (contract 19) — and the report is
// never absent from a state. `installed` stays in the schema's enum with no row carrying it here:
// it was codegraph's until LEGION-629.
test("the state golden carries the deployment's capability report", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));

  const rows = Object.fromEntries(state.capabilities.map((row) => [row.name, row]));
  expect(rows.secrets).toEqual({
    name: "secrets",
    status: "decided",
    detail: "runtime.kubernetes.agent_secrets is not configured",
    decision:
      "pods are enrolled with the secrets broker once dispatch://LEGION-205 lands; until then no pod reads a secret",
  });
  expect(rows["resource-limits"]).toEqual({
    name: "resource-limits",
    status: "open",
    detail:
      "roles without CPU and memory requests and limits under runtime.kubernetes.resources: tester",
    configLine: 'capabilities.decided.resource-limits: "<reason>"',
  });
  expect(rows.codegraph).toEqual({
    name: "codegraph",
    status: "present",
    detail: "checked by the daemon's probe of the worker image, which passed",
  });
  expect(rows.subagents).toEqual({
    name: "subagents",
    status: "unchecked",
    detail:
      "no session has reported yet (dispatch://LEGION-663): dispatches task subagents, each on the model its role configures",
  });
  const statuses = state.capabilities.map((row) => row.status);
  for (const status of ["present", "unchecked", "withheld", "decided", "open"] as const) {
    expect(statuses).toContain(status);
  }

  const emptied = fixture("state-stage3.json") as { capabilities: unknown[] };
  expect(emptied.capabilities).toEqual([]);
});

test("a capability row's status is one the report renders, and the report cannot be dropped", () => {
  const unknownStatus = fixture("state.json") as { capabilities: { status: string }[] };
  unknownStatus.capabilities = [{ ...unknownStatus.capabilities[0], status: "missing" }];
  expect(LegionStateResponse.safeParse(unknownStatus).success).toBeFalse();

  // Contract 16's `live` promised a check to come; since contract 19 a live row is `present`,
  // `open` or `unchecked` from the sessions' reports, and a daemon still rendering `live` is one
  // the plugin was not built against.
  const live = fixture("state.json") as { capabilities: { status: string }[] };
  live.capabilities = [{ ...live.capabilities[0], status: "live" }];
  expect(LegionStateResponse.safeParse(live).success).toBeFalse();

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.capabilities;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();
});

// A session's own report (contract 19): `claims/ready` carries what the session measured of the
// live rows, and the state shows each claim's latest report partitioned into the names that passed
// and the gaps that did not, with the incarnation of the process that reported.
test("the register golden names the task agents the role's prompts dispatch", () => {
  const registration = LegionRegisterResponse.parse(fixture("register.json"));

  expect(registration.promptAgents).toEqual([
    "deep-worker",
    "oracle",
    "plan-gap-analyst",
    "plan-reviewer",
    "thermonuclear-code-quality",
    "thermonuclear-deep-review",
  ]);

  const mutated = fixture("register.json") as Record<string, unknown>;
  delete mutated.promptAgents;
  expect(LegionRegisterResponse.safeParse(mutated).success).toBeFalse();
});

test("the ready golden carries the session's capability report", () => {
  const ready = LegionReadyRequest.parse(fixture("ready.json"));

  expect(ready.capabilities?.rows).toHaveLength(3);
  expect(ready.capabilities?.rows.map((row) => [row.name, row.ok])).toEqual([
    ["subagents", true],
    ["dispatch-envoy-tools", true],
    ["github", false],
  ]);

  // A passing check with nothing to add sends an empty detail; a report is optional on the wire
  // (the controller's ready carries none).
  const bare = fixture("ready.json") as { capabilities: { rows: Record<string, unknown>[] } };
  bare.capabilities.rows = [{ name: "github", ok: true, detail: "" }];
  expect(LegionReadyRequest.safeParse(bare).success).toBeTrue();
  const withoutReport = fixture("ready.json") as Record<string, unknown>;
  delete withoutReport.capabilities;
  expect(LegionReadyRequest.safeParse(withoutReport).success).toBeTrue();
});

test("the state golden shows a claim's capability report beside its locator", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  const implementer = state.issues["LEGION-208"]?.workers.implementer?.claim;

  expect(implementer?.capabilities).toEqual({
    measuredAt: "2026-09-22T09:17:03Z",
    incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5",
    ok: ["subagents", "dispatch-envoy-tools"],
    open: [{ name: "github", detail: "gh api user: HTTP 401: Bad credentials" }],
  });
  expect(implementer?.capabilities?.incarnation).toBe(implementer?.locator?.incarnation);
  // The planner has reported nothing: a claim view without `capabilities` parses.
  expect(state.issues["LEGION-208"]?.workers.planner?.claim.capabilities).toBeUndefined();

  const withoutReport = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, { claim: Record<string, unknown> }> }>;
  };
  delete withoutReport.issues["LEGION-208"]?.workers.implementer?.claim.capabilities;
  expect(LegionStateResponse.safeParse(withoutReport).success).toBeTrue();

  // The implementer's report in a parsed copy of the golden, altered one member at a time.
  const parsedReport = () => {
    const document = LegionStateResponse.parse(fixture("state.json"));
    const report = document.issues["LEGION-208"]?.workers.implementer?.claim.capabilities;
    if (report === undefined) throw new Error("state.json: the implementer has no report");
    return { document, report };
  };

  // A report the API recorded from a claim whose machine held no process names no incarnation.
  const unlocated = parsedReport();
  unlocated.report.incarnation = "";
  expect(LegionStateResponse.safeParse(unlocated.document).success).toBeTrue();

  // The daemon floors an open row's detail (`capabilities.Normalize`); the schema stays the guard.
  const blank = parsedReport();
  blank.report.open = [{ name: "github", detail: "" }];
  expect(LegionStateResponse.safeParse(blank.document).success).toBeFalse();
});

// An operator spawns a claim on an issue no workflow records — `legion claims spawn`, which is
// Stage 2's shape and the merge-queue holder's in production — and state lists that issue for the
// claim's sake. The document parsed here is the one a live daemon served with such a claim on it:
// the whole document was refused, so every strict client lost state reading for as long as that
// claim lived, over a row that is a legitimate state rather than corruption.
test("state carrying an operator's claim on an issue no workflow records is parsed", () => {
  const state = LegionStateResponse.parse(fixture("state-operator-claim.json"));

  const spawned = state.issues["AC91849371-900"];
  expect(spawned?.phase).toBe("unrecorded");
  expect(spawned?.status).toBe("unrecorded");
  expect(spawned?.architect?.state).toBe("idle");
});

test("a locator is the runtime's nested shape, never the flat one", () => {
  const mutated = fixture("operator-claim.json") as { locator: Record<string, unknown> };
  mutated.locator = {
    runtime: "tmux",
    claim: "legion-legion-legion-209-implementer",
    incarnation: "40217:9551230",
    window: "@7",
    pane: "%23",
  };

  expect(LegionOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a locator's backend member is the one its runtime names", () => {
  const mutated = fixture("operator-claim.json") as { locator: Record<string, unknown> };
  mutated.locator = { ...mutated.locator, runtime: "sandbox" };

  expect(LegionOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a claim state is one the supervisor has", () => {
  const mutated = fixture("operator-claim.json") as Record<string, unknown>;
  mutated.state = "running";

  expect(LegionOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a register response without its secret is refused", () => {
  const mutated = fixture("register.json") as Record<string, unknown>;
  delete mutated.secret;

  expect(LegionRegisterResponse.safeParse(mutated).success).toBeFalse();
});

test("a claim's registration and the controller's are told apart by their schemas", () => {
  const claim = fixture("register.json");
  const controller = fixture("register-controller.json");

  expect(LegionControllerRegisterResponse.safeParse(controller).success).toBeTrue();
  expect(LegionRegisterResponse.safeParse(controller).success).toBeFalse();
  expect(LegionControllerRegisterResponse.safeParse(claim).success).toBeFalse();
});
