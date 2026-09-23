import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  LegionGoEmptyResponse,
  LegionGoErrorResponse,
  LegionGoGateRegisterRequest,
  LegionGoGitCredentialResponse,
  LegionGoGitHubTokenResponse,
  LegionGoGrantCredentialRequest,
  LegionGoGrantRequest,
  LegionGoGrantResponse,
  LegionGoHandoffCompleteRequest,
  LegionGoIssueStatusRequest,
  LegionGoOperatorClaimResponse,
  LegionGoOperatorClaimsResponse,
  LegionGoPhaseBackwardRequest,
  LegionGoPhaseRetryRequest,
  LegionGoRegisterResponse,
  LegionGoSignOffRequest,
  LegionGoStateResponse,
  LegionGoWaveReleaseRequest,
  LegionGoWaveReleaseResponse,
} from "./legion-go-api";

const fixtureDir = path.join(import.meta.dir, "..", "fixtures", "daemon-api");

/** Every fixture the Go golden test writes, with the schema that must accept it. A new fixture
 * with no entry here fails the first test rather than going unchecked. */
const schemas: Record<string, z.ZodType> = {
  "state.json": LegionGoStateResponse,
  "state-stage3.json": LegionGoStateResponse,
  "register.json": LegionGoRegisterResponse,
  "error.json": LegionGoErrorResponse,
  "operator-claim.json": LegionGoOperatorClaimResponse,
  "operator-claims.json": LegionGoOperatorClaimsResponse,
  "grant.json": LegionGoGrantResponse,
  "github-token.json": LegionGoGitHubTokenResponse,
  "git-credential.json": LegionGoGitCredentialResponse,
  "provisioning-credential.json": LegionGoGitHubTokenResponse,
  "handoff-complete.json": LegionGoEmptyResponse,
  "issue-status.json": LegionGoEmptyResponse,
  "gate-register.json": LegionGoEmptyResponse,
  "wave-release.json": LegionGoWaveReleaseResponse,
  "phase-backward.json": LegionGoEmptyResponse,
  "phase-retry.json": LegionGoEmptyResponse,
  "signoff.json": LegionGoEmptyResponse,
  // No route answers this one: it is the contract number the Go daemon's boot gate requires of the
  // installed plugin (`internal/api/version.go`), and the plugin's own test pins its manifest's
  // `legion.goDaemonApiVersion` to it.
  "version.json": z.strictObject({ goDaemonApiVersion: z.number().int().positive() }),
};

function fixture(name: string): unknown {
  return JSON.parse(readFileSync(path.join(fixtureDir, name), "utf8"));
}

test("every Go-written fixture parses through the strict schema", () => {
  const names = readdirSync(fixtureDir).filter((name) => name.endsWith(".json"));
  expect(names.length).toBeGreaterThan(0);

  for (const name of names) {
    const schema = schemas[name];
    expect(schema, `${name} has no schema in legion-go-api.test.ts`).toBeDefined();
    const parsed = schema?.safeParse(fixture(name));
    expect(parsed?.error?.issues ?? [], `${name} failed the schema`).toEqual([]);
  }
});

test("every Stage 3 workflow request has a strict schema", () => {
  const requests: ReadonlyArray<readonly [string, z.ZodType, Record<string, unknown>]> = [
    [
      "grant",
      LegionGoGrantRequest,
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-209" },
    ],
    ["grant credential", LegionGoGrantCredentialRequest, { grantId: "grant-208" }],
    [
      "handoff complete",
      LegionGoHandoffCompleteRequest,
      { grantId: "grant-208", summary: "completed", verdict: "", ready: false, commit: "abc123" },
    ],
    [
      "issue status",
      LegionGoIssueStatusRequest,
      { grantId: "grant-208", issue: "LEGION-208", status: "todo" },
    ],
    [
      "gate register",
      LegionGoGateRegisterRequest,
      {
        grantId: "grant-208",
        issue: "LEGION-208",
        artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
        version: 7,
      },
    ],
    ["wave release", LegionGoWaveReleaseRequest, { grantId: "grant-208", issues: ["LEGION-209"] }],
    [
      "phase backward",
      LegionGoPhaseBackwardRequest,
      { grantId: "grant-208", to: "implementing", reason: "test failed" },
    ],
    [
      "phase retry",
      LegionGoPhaseRetryRequest,
      { grantId: "grant-208", issue: "LEGION-208", decision: "retry" },
    ],
    ["signoff", LegionGoSignOffRequest, { grantId: "grant-208", issue: "LEGION-208" }],
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

test("a workflow refusal preserves its stable code and message", () => {
  expect(
    LegionGoErrorResponse.safeParse({
      code: "GATE_UNAPPROVED",
      error: "the current spec version needs approval",
    }).success
  ).toBeTrue();
});

test("a field the Go shape does not carry is refused", () => {
  const mutated = fixture("state.json") as Record<string, unknown>;
  mutated.workerAdmission = { queue: [] };

  expect(LegionGoStateResponse.safeParse(mutated).success).toBeFalse();
});

test("a field the Go shape requires cannot be dropped", () => {
  const mutated = fixture("state.json") as { admission: Record<string, unknown> };
  delete mutated.admission.waiting;

  expect(LegionGoStateResponse.safeParse(mutated).success).toBeFalse();
});

test("the Stage 3 state golden carries the record status and due Dispatch status write", () => {
  const state = LegionGoStateResponse.parse(fixture("state-stage3.json"));

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

  expect(LegionGoStateResponse.safeParse(state).success).toBeFalse();
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

  expect(LegionGoOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a locator's backend member is the one its runtime names", () => {
  const mutated = fixture("operator-claim.json") as { locator: Record<string, unknown> };
  mutated.locator = { ...mutated.locator, runtime: "sandbox" };

  expect(LegionGoOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a claim state is one the supervisor has", () => {
  const mutated = fixture("operator-claim.json") as Record<string, unknown>;
  mutated.state = "running";

  expect(LegionGoOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a register response without its secret is refused", () => {
  const mutated = fixture("register.json") as Record<string, unknown>;
  delete mutated.secret;

  expect(LegionGoRegisterResponse.safeParse(mutated).success).toBeFalse();
});
