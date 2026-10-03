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
  LegionGitCredentialResponse,
  LegionGitHubTokenResponse,
  LegionGrantCredentialRequest,
  LegionGrantRequest,
  LegionGrantResponse,
  LegionHandoffCompleteRequest,
  LegionIssueStatusRequest,
  LegionOperatorClaimResponse,
  LegionOperatorClaimsResponse,
  LegionPhaseBackwardRequest,
  LegionPhaseRetryRequest,
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
  "register.json": LegionRegisterResponse,
  "register-controller.json": LegionControllerRegisterResponse,
  "controller-secret.json": LegionControllerSecretResponse,
  "error.json": LegionErrorResponse,
  "operator-claim.json": LegionOperatorClaimResponse,
  "operator-claims.json": LegionOperatorClaimsResponse,
  "grant.json": LegionGrantResponse,
  "github-token.json": LegionGitHubTokenResponse,
  "git-credential.json": LegionGitCredentialResponse,
  "provisioning-credential.json": LegionGitHubTokenResponse,
  "handoff-complete.json": LegionEmptyResponse,
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
    ["grant credential", LegionGrantCredentialRequest, { grantId: "grant-208" }],
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
