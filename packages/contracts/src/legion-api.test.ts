import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  LEGION_PHASE_BACKWARD_TARGETS,
  LEGION_PHASES,
  LEGION_WORKFLOW_PHASES,
  LegionCapabilityReport,
  LegionChildRequest,
  LegionControllerCredentialResponse,
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
  LegionReadyForUpdateRequest,
  LegionReadyRequest,
  LegionRegisterResponse,
  LegionRootCloseRequest,
  LegionSignOffRequest,
  LegionStateResponse,
  LegionWaveReleaseRequest,
  LegionWaveReleaseResponse,
} from "./legion-api";
import { LEGION_ROLES } from "./legion-roles";

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
  // The claim wire's `claim.ReadyRequest` with a capability report: what a session posts on
  // `claims/ready` (contract 21).
  "ready.json": LegionReadyRequest,
  // `claim.ReadyForUpdateRequest`: the worker's answer to its pod's update task, saying it has
  // pushed everything it wants kept and its process may be replaced.
  "ready-for-update.json": LegionReadyForUpdateRequest,
  "register-controller.json": LegionControllerRegisterResponse,
  "controller-secret.json": LegionControllerSecretResponse,
  "controller-github-credential.json": LegionControllerCredentialResponse,
  "error.json": LegionErrorResponse,
  "operator-claim.json": LegionOperatorClaimResponse,
  "operator-forget-session.json": LegionOperatorClaimResponse,
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

function isJsonObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** A golden read as the JSON object a route's body is. */
function fixtureBody(name: string): Record<string, unknown> {
  const body = fixture(name);
  if (!isJsonObject(body)) {
    throw new Error(`${name} is not a JSON object`);
  }
  return body;
}

/** The path of the first key the parse dropped: the first key present in `file` and absent from
 * `parsed`, descending objects by key and arrays by index. A Go field with no line in its schema
 * reads as its own path — `issues.LEGION-207.workers.implementer.claim.somethingNew` — rather
 * than as a diff of two large documents. */
function firstDroppedKey(file: unknown, parsed: unknown, path = ""): string | undefined {
  if (Array.isArray(file) && Array.isArray(parsed)) {
    for (const [index, element] of file.entries()) {
      const dropped = firstDroppedKey(element, parsed[index], `${path}[${index}]`);
      if (dropped !== undefined) {
        return dropped;
      }
    }
    return undefined;
  }
  if (!isJsonObject(file) || !isJsonObject(parsed)) {
    return undefined;
  }
  for (const [key, value] of Object.entries(file)) {
    const keyPath = path === "" ? key : `${path}.${key}`;
    if (!(key in parsed)) {
      return keyPath;
    }
    const dropped = firstDroppedKey(value, parsed[key], keyPath);
    if (dropped !== undefined) {
      return dropped;
    }
  }
  return undefined;
}

/** What the gate below reads of one state document: the path of the first key its parse dropped. */
function droppedFromState(state: unknown): string | undefined {
  return firstDroppedKey(state, LegionStateResponse.parse(state));
}

// The drift gate. The answer schemas strip a key they do not name, so a parse that reports no
// problem proves nothing on its own: a Go field with no line in the schema is dropped in silence
// and the parse still succeeds. Every key of a Go-written file has a line in its schema exactly
// when the parse equals the file by value, so that equality is the gate. By value and not by
// text: a stripping object rebuilds a value's keys in its shape's order, so a key-order
// comparison of the state goldens would be red before any drift.
test("every Go-written fixture parses to a value equal to the file", () => {
  const names = readdirSync(fixtureDir).filter((name) => name.endsWith(".json"));
  expect(names.length).toBeGreaterThan(0);

  for (const name of names) {
    const schema = schemas[name];
    expect(schema, `${name} has no schema in legion-api.test.ts`).toBeDefined();
    const file = fixture(name);
    const parsed = schema?.safeParse(file);
    expect(parsed?.error?.issues ?? [], `${name} failed the schema`).toEqual([]);
    const dropped = firstDroppedKey(file, parsed?.data);
    expect(dropped, `${name}: the Go field ${dropped} has no line in its schema`).toBeUndefined();
    expect(parsed?.data, `${name} parses to a value the file does not equal`).toEqual(file);
  }
});

// What that equality catches, and the one place it is blind. A key no schema names is dropped
// wherever an object names its keys, at the top level and at the depth the 2026-11-03 incident
// took, and the gate names its path. Under `settings` (a `z.looseObject`) and under a pending
// status write's `payload` (a `z.record`) nothing is dropped, so the gate sees nothing there:
// both blocks are loose by design — legion.yaml's keys and the workflow's payload are not this
// wire's shape — and that is the limit of what the gate can hold.
test("the gate names a key no schema names, and is blind under the loose blocks", () => {
  const topLevel = fixtureBody("state.json");
  topLevel.workerAdmission = { queue: [] };
  expect(droppedFromState(topLevel)).toBe("workerAdmission");

  const underClaim = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, { claim: Record<string, unknown> }> }>;
  };
  const claim = underClaim.issues["LEGION-207"]?.workers.implementer?.claim;
  if (claim === undefined) {
    throw new Error("state fixture lacks LEGION-207's implementer claim");
  }
  claim.somethingNew = "a field the Go view grew";
  expect(droppedFromState(underClaim)).toBe(
    "issues.LEGION-207.workers.implementer.claim.somethingNew"
  );

  const settings = fixture("state.json") as { settings: Record<string, unknown> };
  settings.settings.a_later_legion_yaml_key = "ridden loosely";
  expect(droppedFromState(settings)).toBeUndefined();
  expect<unknown>(LegionStateResponse.parse(settings)).toEqual(settings);

  const payload = fixture("state-stage3.json") as {
    pendingStatusWrites: { payload: Record<string, unknown> }[];
  };
  const write = payload.pendingStatusWrites[0];
  if (write === undefined) {
    throw new Error("state-stage3 fixture lacks its pending status write");
  }
  write.payload.somethingNew = "the workflow owns this shape";
  expect(droppedFromState(payload)).toBeUndefined();
  expect<unknown>(LegionStateResponse.parse(payload)).toEqual(payload);
});

// `workers` is keyed by a plain string, so the schema no longer holds the role names and the gate
// above cannot see a role the Go side adds. The key list it held is held here instead: every
// worker key of every golden is one of this build's roles.
test("every golden's worker keys are roles this build knows", () => {
  const knownRoles: readonly string[] = LEGION_ROLES;
  const states = Object.keys(schemas).filter((name) => schemas[name] === LegionStateResponse);
  expect(states.length).toBeGreaterThan(0);

  let seen = 0;
  for (const name of states) {
    const state = LegionStateResponse.parse(fixture(name));
    for (const [key, issue] of Object.entries(state.issues)) {
      for (const role of Object.keys(issue.workers)) {
        expect(knownRoles, `${name}: ${key} carries the worker ${role}`).toContain(role);
        seen += 1;
      }
    }
  }
  expect(seen).toBeGreaterThan(0);
});

// A role key this build does not know arrives under `issues.<key>.workers` the day the daemon
// grows a role. It used to refuse the whole document, so every reader lost its state over a key
// it merely did not know; now the unknown entry is read beside the known roles, whose views come
// through unchanged.
test("a worker role this build does not know leaves the rest of the state readable", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, unknown> }>;
  };
  const workers = state.issues["LEGION-208"]?.workers;
  if (workers?.planner === undefined || workers.implementer === undefined) {
    throw new Error("state fixture lacks LEGION-208's planner and implementer");
  }
  workers.auditor = workers.planner;

  const parsed = LegionStateResponse.parse(state).issues["LEGION-208"]?.workers;
  expect<unknown>(parsed?.planner).toEqual(workers.planner);
  expect<unknown>(parsed?.implementer).toEqual(workers.implementer);
  expect<unknown>(parsed?.auditor).toEqual(workers.planner);
});

/** What zod 4.3.6 marks an object node with, read rather than built: a stripping object has no
 * `catchall`, a refusing one a `never`, a loose one an `unknown`. */
type ObjectMark = "strips" | "refuses" | "loose";

function objectMark(node: z.ZodObject): ObjectMark {
  const catchall = node.def.catchall?._zod.def.type;
  if (catchall === undefined) {
    return "strips";
  }
  return catchall === "never" ? "refuses" : "loose";
}

/** The node kinds the answer schemas are built from that hold no object below them. A kind
 * outside this list and the walk's own branches throws: a `z.tuple`, a `z.intersection`, a
 * `.pipe()` or a `z.lazy()` added later would otherwise take its whole subtree out of the census
 * in silence. */
const CENSUS_LEAF_KINDS: Record<string, true> = {
  boolean: true,
  enum: true,
  literal: true,
  number: true,
  string: true,
  unknown: true,
};

/** Every object node of the given schemas, by mark and by path. The walk descends an object's
 * shape, an array's element (`[]`), a record's value type (`{}`), a union's options (`|0`, `|1`;
 * `.refine()` wraps nothing, so a refined object is still an object node and a refined
 * discriminated union still a union node) and the inner type of an optional or a nullable. A node
 * two paths reach is one node, counted at the first path that reaches it: the claim view under
 * `architect`, the cache view under `repositories.mapped[].cache`. */
function objectCensus(roots: Record<string, z.ZodType>): Record<ObjectMark, string[]> {
  const found: Record<ObjectMark, string[]> = { strips: [], refuses: [], loose: [] };
  const seen = new Set<z.core.$ZodType>();

  const walk = (node: z.core.$ZodType, path: string): void => {
    if (seen.has(node)) {
      return;
    }
    seen.add(node);
    if (node instanceof z.ZodObject) {
      found[objectMark(node)].push(path);
      for (const [key, value] of Object.entries(node.def.shape)) {
        walk(value, `${path}.${key}`);
      }
      return;
    }
    if (node instanceof z.ZodArray) {
      walk(node.def.element, `${path}[]`);
      return;
    }
    if (node instanceof z.ZodRecord) {
      walk(node.def.valueType, `${path}{}`);
      return;
    }
    if (node instanceof z.ZodUnion) {
      for (const [index, option] of node.def.options.entries()) {
        walk(option, `${path}|${index}`);
      }
      return;
    }
    if (node instanceof z.ZodOptional || node instanceof z.ZodNullable) {
      walk(node.def.innerType, path);
      return;
    }
    const kind = node._zod.def.type;
    if (CENSUS_LEAF_KINDS[kind] !== true) {
      throw new Error(`the census reached a ${kind} node it does not walk, at ${path}`);
    }
  };

  for (const [name, schema] of Object.entries(roots)) {
    walk(schema, name);
  }
  for (const paths of Object.values(found)) {
    paths.sort();
  }
  return found;
}

// The whole answer side strips, not only what a golden passes through: a body reaches the eight
// or so objects its own route carries, so a partial edit that left one nested view refusing would
// pass every other case here. The census walks the twelve answer schemas and every node below
// them and reads zod's own mark on each object, and it names the paths beside the counts, so a
// node that appeared or vanished is named rather than counted.
test("every answer-side object strips a key it does not name", () => {
  // The mark first, on three nodes built here: were a zod release to stop setting `catchall`,
  // every node would read as stripping and the census below would pass proving nothing.
  expect({
    strict: objectMark(z.strictObject({})),
    stripping: objectMark(z.object({})),
    loose: objectMark(z.looseObject({})),
  }).toEqual({ strict: "refuses", stripping: "strips", loose: "loose" });

  const found = objectCensus({
    LegionStateResponse,
    LegionRegisterResponse,
    LegionControllerRegisterResponse,
    LegionControllerSecretResponse,
    LegionControllerCredentialResponse,
    LegionOperatorClaimResponse,
    LegionOperatorClaimsResponse,
    LegionGrantResponse,
    LegionHandoffCompleteResponse,
    LegionWaveReleaseResponse,
    LegionEmptyResponse,
    LegionErrorResponse,
  });

  expect(found.refuses).toEqual([]);
  // `settings` is the one loose block (contract 35). A pending status write's `payload` is a
  // record, not an object, and is no node of this census.
  expect(found.loose).toEqual(["LegionStateResponse.settings"]);
  expect(found.strips).toEqual([
    "LegionControllerCredentialResponse",
    "LegionControllerCredentialResponse.owners{}",
    "LegionControllerRegisterResponse",
    "LegionControllerSecretResponse",
    "LegionEmptyResponse",
    "LegionErrorResponse",
    "LegionGrantResponse",
    "LegionHandoffCompleteResponse",
    "LegionOperatorClaimResponse",
    "LegionOperatorClaimResponse.budgets",
    "LegionOperatorClaimResponse.pending",
    "LegionOperatorClaimsResponse",
    "LegionRegisterResponse",
    "LegionStateResponse",
    "LegionStateResponse.admission",
    "LegionStateResponse.admission.blocked[]",
    "LegionStateResponse.agentSecretsLogin",
    "LegionStateResponse.capabilities[]",
    "LegionStateResponse.consumers[]",
    "LegionStateResponse.controllerLocator",
    "LegionStateResponse.daemon",
    "LegionStateResponse.heldOutboxRows[]",
    "LegionStateResponse.issues{}",
    "LegionStateResponse.issues{}.architect",
    "LegionStateResponse.issues{}.architect.capabilities",
    "LegionStateResponse.issues{}.architect.capabilities.open[]",
    "LegionStateResponse.issues{}.architect.locator|0",
    "LegionStateResponse.issues{}.architect.locator|0.tmux",
    "LegionStateResponse.issues{}.architect.locator|1",
    "LegionStateResponse.issues{}.architect.locator|1.sandbox",
    "LegionStateResponse.issues{}.check",
    "LegionStateResponse.issues{}.check.lastWake",
    "LegionStateResponse.issues{}.designGate",
    "LegionStateResponse.issues{}.pullRequest",
    "LegionStateResponse.issues{}.slot",
    "LegionStateResponse.issues{}.waitingOn|0",
    "LegionStateResponse.issues{}.waitingOn|1",
    "LegionStateResponse.issues{}.workers{}",
    "LegionStateResponse.issues{}.workers{}.handoffRecord",
    "LegionStateResponse.pendingStatusWrites[]",
    "LegionStateResponse.repositories",
    "LegionStateResponse.repositories.mapped[]",
    "LegionStateResponse.repositories.mapped[].cache",
    "LegionStateResponse.repositories.refused[]",
    "LegionStateResponse.repositories.withoutWebhookApp[]",
    "LegionStateResponse.strandedClaims[]",
    "LegionWaveReleaseResponse",
  ]);
  expect({
    strips: found.strips.length,
    loose: found.loose.length,
    refuses: found.refuses.length,
    nodes: found.strips.length + found.loose.length + found.refuses.length,
  }).toEqual({ strips: 47, loose: 1, refuses: 0, nodes: 48 });
});

test("state accepts optional fields emitted by later workflow slices", () => {
  const current = fixture("state.json");
  expect(LegionStateResponse.safeParse(current).success).toBeTrue();

  const later = fixture("state.json") as {
    admission: Record<string, unknown>;
    issues: Record<string, { phase: string; slot?: Record<string, unknown> }>;
  };
  const laterIssue = later.issues["LEGION-208"];
  if (laterIssue === undefined || laterIssue.slot === undefined) {
    throw new Error("state fixture lacks LEGION-208 or its slot");
  }
  later.admission.free = 1;
  laterIssue.phase = "integrating";
  laterIssue.slot.lentTo = "LEGION-209";

  expect(LegionStateResponse.safeParse(later).success).toBeTrue();
});

test("state rejects malformed optional workflow fields", () => {
  const negativeFree = fixture("state.json") as { admission: Record<string, unknown> };
  negativeFree.admission.free = -1;
  expect(LegionStateResponse.safeParse(negativeFree).success).toBeFalse();

  const malformedLender = fixture("state.json") as {
    issues: Record<string, { slot?: Record<string, unknown> }>;
  };
  const malformedLenderIssue = malformedLender.issues["LEGION-208"];
  if (malformedLenderIssue === undefined || malformedLenderIssue.slot === undefined) {
    throw new Error("state fixture lacks LEGION-208 or its slot");
  }
  malformedLenderIssue.slot.lentTo = "not an issue key";
  expect(LegionStateResponse.safeParse(malformedLender).success).toBeFalse();
});

test("state accepts the existing done phase", () => {
  const completed = fixture("state.json") as {
    issues: Record<string, { phase: string }>;
  };
  const completedIssue = completed.issues["LEGION-208"];
  if (completedIssue === undefined) {
    throw new Error("state fixture lacks LEGION-208");
  }
  completedIssue.phase = "done";

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

// Contract 40: `awaiting_person`, a root that gave its slot up while its architect waits on a
// person, follows `admitted`, and no backward move reaches it. Contract 33: the architect accepts
// before the merge, in `accepting`, between `reviewing` and `retro`; the merge closes the issue
// (`done`), and the production check after the close is no phase, so no phase follows
// `awaiting_merge` and `integrating` but the close itself.
test("the phase lists are the daemon's, with awaiting_person after admitted and accepting before retro", () => {
  expect(LEGION_WORKFLOW_PHASES).toEqual([
    "admitted",
    "awaiting_person",
    "planning",
    "implementing",
    "testing",
    "reviewing",
    "accepting",
    "retro",
    "merging",
    "awaiting_merge",
    "integrating",
    "done",
    "held",
  ]);
  expect(LEGION_PHASE_BACKWARD_TARGETS).toEqual([
    "admitted",
    "planning",
    "implementing",
    "testing",
    "reviewing",
    "accepting",
    "retro",
    "merging",
    "awaiting_merge",
    "done",
    "held",
  ]);
  expect(LEGION_PHASES).toEqual([...LEGION_WORKFLOW_PHASES, "unrecorded"]);
  expect(
    LegionPhaseBackwardRequest.safeParse({
      grantId: "grant-208",
      to: "accepting",
      reason: "retro reopened the review",
    }).success
  ).toBeTrue();
});

// Contract 40: the state golden carries LEGION-212, a root waiting on a person whose question the
// person answered. Both shapes of `waitingOn` parse, an answer comes with the act that gave it,
// and a wait half answered, an act no answer names, or a `kind` no shape has is refused.
test("the state golden carries what an awaiting_person root waits on", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { phase: string; waitingOn?: Record<string, unknown> }>;
  };
  const waiting = state.issues["LEGION-212"];
  if (waiting === undefined || waiting.waitingOn === undefined) {
    throw new Error("state fixture lacks LEGION-212 or its waitingOn");
  }
  expect(waiting.phase).toBe("awaiting_person");
  expect(waiting.waitingOn).toEqual({
    kind: "ask",
    ask: "9a8fd348-55d5-4d72-a793-3cd037f75f13",
    since: "2026-09-22T08:05:00Z",
    answeredAt: "2026-09-22T09:21:00Z",
    answeredBy: "ask",
  });

  const parses = (waitingOn: Record<string, unknown>) => {
    waiting.waitingOn = waitingOn;
    return LegionStateResponse.safeParse(state).success;
  };
  expect(parses({ kind: "ask", ask: "ask-7" })).toBeTrue();
  expect(parses({ kind: "gate", gateVersion: 3, artifactId: "spec-doc" })).toBeTrue();
  expect(
    parses({
      kind: "gate",
      gateVersion: 3,
      artifactId: "spec-doc",
      answeredAt: "2026-09-22T09:21:00Z",
      answeredBy: "approved",
    })
  ).toBeTrue();
  expect(parses({ kind: "ask", ask: "ask-7", answeredAt: "2026-09-22T09:21:00Z" })).toBeFalse();
  expect(parses({ kind: "ask", ask: "ask-7", answeredBy: "ask" })).toBeFalse();
  expect(
    parses({ kind: "ask", ask: "ask-7", answeredAt: "2026-09-22T09:21:00Z", answeredBy: "guess" })
  ).toBeFalse();
  expect(parses({ kind: "gate", gateVersion: 3 })).toBeFalse();
  expect(parses({ kind: "other", ask: "ask-7" })).toBeFalse();
});

// The state golden carries LEGION-207, closed by its merge, with its production check open: the
// repository, pull request and merge commit the check looks for, its looks, the wake that started
// the latest and the bound. The check is present only while open, so an issue without one parses,
// and a wake with no kind is refused.
test("the state golden carries a done issue's open production check", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { phase: string; check?: Record<string, unknown> }>;
  };
  const closed = state.issues["LEGION-207"];
  if (closed === undefined || closed.check === undefined) {
    throw new Error("state fixture lacks LEGION-207 or its check");
  }
  expect(closed.phase).toBe("done");
  expect(closed.check).toEqual({
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
  });

  // Before the first look wakes there is no wake, and a merge commit the daemon never recorded is
  // empty until the first build compare fills it.
  const { lastWake: _, ...unwoken } = closed.check;
  closed.check = { ...unwoken, mergeCommit: "", looks: 0, lookOpen: false };
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);

  closed.check = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);

  closed.check = { ...unwoken, lastWake: { kind: "", detail: "", at: "2026-09-22T09:10:51Z" } };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
});

// The implementer's check report carries no commit (the look pushed nothing) and a verdict the
// daemon holds to pass, pending or fail; a phase completion keeps its commit.
test("a check report is a completion without a commit", () => {
  const report = { grantId: "grant-207", summary: "Driven in production.", ready: false };
  for (const verdict of ["pass", "pending", "fail"]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...report, verdict }).success).toBeTrue();
  }
  expect(
    LegionHandoffCompleteRequest.safeParse({ ...report, verdict: "pass", commit: "" }).success
  ).toBeFalse();
});

// A file-backed phase's completion names the Dispatch handoff record the worker wrote: the
// planner, tester and reviewer report it alone, the implementer with the commit it pushed. A record
// is named by a non-empty id and a positive version.
test("a phase completion names its handoff record", () => {
  const planner = {
    grantId: "grant-208",
    summary: "Planned.",
    verdict: "",
    ready: false,
    handoffId: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b",
    handoffVersion: 1,
  };
  expect(LegionHandoffCompleteRequest.safeParse(planner).success).toBeTrue();
  expect(
    LegionHandoffCompleteRequest.safeParse({ ...planner, commit: "abc123" }).success
  ).toBeTrue();
  expect(LegionHandoffCompleteRequest.safeParse({ ...planner, handoffId: "" }).success).toBeFalse();
  for (const handoffVersion of [0, -1, 1.5]) {
    expect(
      LegionHandoffCompleteRequest.safeParse({ ...planner, handoffVersion }).success
    ).toBeFalse();
  }
});

// The state shows the record a worker's completion of its current file-backed phase named, its id
// and positive version; a worker that named none shows no field.
test("the state golden carries a worker's handoff record", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, { handoffRecord?: unknown }> }>;
  };
  const planner = state.issues["LEGION-208"]?.workers.planner;
  if (planner === undefined) {
    throw new Error("state fixture lacks LEGION-208's planner");
  }
  expect(planner.handoffRecord).toEqual({ id: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", version: 1 });

  planner.handoffRecord = { id: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", version: 0 };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  planner.handoffRecord = { id: "", version: 1 };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  planner.handoffRecord = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);
});

// An architect's backward move names the issue it moves from accepting; a phase worker's may
// leave it out, since its grant names its own.
test("a backward move may name its issue", () => {
  const move = { grantId: "grant-208", to: "testing", reason: "the proof is thin" };
  expect(LegionPhaseBackwardRequest.safeParse(move).success).toBeTrue();
  expect(LegionPhaseBackwardRequest.safeParse({ ...move, issue: "LEGION-209" }).success).toBeTrue();
  expect(LegionPhaseBackwardRequest.safeParse({ ...move, issue: "" }).success).toBeFalse();
});

test("every request the plugin sends has a strict schema", () => {
  const ready = fixtureBody("ready.json");
  const readyForUpdate = fixtureBody("ready-for-update.json");
  const report = LegionCapabilityReport.parse(ready.capabilities);
  const loosenedReport = {
    ...report,
    rows: report.rows.map((row) => ({ ...row, unexpected: true })),
  };
  // A route, a body it must accept, and — where the row guards a nested object — that body with
  // one key no schema names inside the nested object; a row without one has the key added at the
  // top level.
  const requests: ReadonlyArray<
    readonly [string, z.ZodType, Record<string, unknown>, Record<string, unknown>?]
  > = [
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
      {
        grantId: "grant-208",
        summary: "completed",
        verdict: "",
        ready: false,
        commit: "abc123",
        handoffId: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b",
        handoffVersion: 2,
      },
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
    // The claim wire's own bodies (contracts 21 and 46), read from the goldens the Go side
    // writes rather than invented here.
    ["ready", LegionReadyRequest, ready],
    ["ready for update", LegionReadyForUpdateRequest, readyForUpdate],
    ["capability report", LegionCapabilityReport, report],
    // The unknown key inside the report's rows: the nested object a partial edit of the request
    // side would have loosened unseen.
    ["capability report row", LegionCapabilityReport, report, loosenedReport],
  ];

  for (const [name, schema, request, loosened] of requests) {
    expect(schema.safeParse(request).success, `${name} accepts its route request`).toBeTrue();
    expect(
      schema.safeParse(loosened ?? { ...request, unexpected: true }).success,
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

// `LegionEmptyResponse` names no field, so it accepts any JSON object a fact route answers; a
// field a fact route grows is caught by the drift gate above, not here. What it refuses is a body
// that is not an object at all.
test("the empty answer refuses a body that is not an object", () => {
  expect(LegionEmptyResponse.safeParse({}).success).toBeTrue();
  expect(LegionEmptyResponse.safeParse({ note: "ignored" }).success).toBeTrue();
  expect(LegionEmptyResponse.safeParse([]).success).toBeFalse();
  expect(LegionEmptyResponse.safeParse("{}").success).toBeFalse();
});

test("a field the Go shape requires cannot be dropped", () => {
  const mutated = fixture("state.json") as { admission: Record<string, unknown> };
  delete mutated.admission.waiting;

  expect(LegionStateResponse.safeParse(mutated).success).toBeFalse();
});

// Contract 38: the state names every issue Legion is waiting to start behind a Dispatch issue
// nobody has finished — a child held at `admitted` and a waiting root alike — by issue key, each
// with the keys it waits on, among them keys the state's `issues` does not record. `blocked` is
// required, so a daemon older than the plugin cannot leave it out, and `waiting` keeps its own
// meaning beside it: the roots' waiting line.
test("the state golden names what every blocked issue waits on", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));

  expect(state.admission.blocked).toEqual([
    { issue: "LEGION-209", blockedBy: ["LEGION-210", "LEGION-213"] },
    { issue: "LEGION-211", blockedBy: ["LEGION-210"] },
  ]);
  expect(state.admission.waiting).toEqual(["LEGION-211"]);

  const dropped = fixture("state.json") as { admission: Record<string, unknown> };
  delete dropped.admission.blocked;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();

  const malformed = fixture("state.json") as { admission: { blocked: unknown[] } };
  malformed.admission.blocked = [{ issue: "LEGION-209" }];
  expect(LegionStateResponse.safeParse(malformed).success).toBeFalse();
});

// Contract 38: a release is accepted whatever its children's links say, so the answer says which
// of them it started and which it left held, with what each held child waits on. Both are
// required, and `held` carries the same `{issue, blockedBy}` row the state's `blocked` does.
test("a wave release answers what it started and what it held", () => {
  expect(LegionWaveReleaseResponse.parse(fixture("wave-release.json"))).toEqual({
    released: ["LEGION-209"],
    started: [],
    held: [],
  });
  expect(
    LegionWaveReleaseResponse.safeParse({
      released: ["LEGION-209", "LEGION-210"],
      started: ["LEGION-210"],
      held: [{ issue: "LEGION-209", blockedBy: ["LEGION-300"] }],
    }).success
  ).toBeTrue();
  expect(LegionWaveReleaseResponse.safeParse({ released: ["LEGION-209"] }).success).toBeFalse();
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

// Contract 31: every recorded issue carries the repository it targets and its default branch, its
// pull request the repository it is in, and the state the project's repository mapping as the
// daemon serves it, each left-out repository named with the App it lacks. Contract 32: each mapped
// repository carries its cache under the Kubernetes runtime, Ready with its last health read or
// the read's failure, and the caches of repositories no longer mapped are listed with when they go.
// A state without the mapping, or without its caches, is refused.
test("the state golden carries each issue's repository and the project's mapping", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));

  expect(state.issues["LEGION-208"]?.repo).toBe("acme/widgets");
  expect(state.issues["LEGION-208"]?.pullRequest?.repo).toBe("acme/widgets");
  expect(state.issues["LEGION-210"]?.defaultBranch).toBe("release/v7.1");
  expect(state.repositories.owners).toEqual(["acme", "zeta"]);
  expect(
    state.repositories.mapped.map(({ repo, default: isDefault }) => ({ repo, default: isDefault }))
  ).toEqual([
    { repo: "acme/widgets", default: true },
    { repo: "zeta/tools", default: false },
  ]);
  expect(state.repositories.mapped[0]?.cache).toEqual({
    repo: "acme/widgets",
    sandbox: "legion-legion-cache-acme-widgets-d782c874",
    state: "ready",
    ready: true,
    address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
    head: "34a7ac80e9c3389ffbc1bbeece371ccacc043992",
    lastFetchAt: "2026-10-16T19:20:30Z",
    bytes: 6612345678,
  });
  expect(state.repositories.mapped[1]?.cache).toEqual({
    repo: "zeta/tools",
    sandbox: "legion-legion-cache-zeta-tools-1b2c3d4e",
    state: "failing",
    reason:
      "container cache CrashLoopBackOff: back-off 5m0s restarting failed container (restarted 4 times)",
    ready: false,
  });
  expect(state.repositories.refused[0]?.reason).toBe(
    "the review App is not installed on acme/gadgets"
  );
  expect(state.repositories.caches).toEqual([
    {
      repo: "acme/legacy",
      sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d",
      state: "starting",
      ready: false,
      retainedUntil: "2026-10-23T09:00:00Z",
    },
  ]);

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.repositories;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();
  const noCaches = fixture("state.json") as { repositories: Record<string, unknown> };
  delete noCaches.repositories.caches;
  expect(LegionStateResponse.safeParse(noCaches).success).toBeFalse();
  const refused = fixture("state.json") as {
    repositories: { caches: Array<Record<string, unknown>> };
  };
  Object.assign(refused.repositories.caches[0] ?? {}, {
    state: "refused",
    reason: "its Service legion-legion-cache-acme-legacy-0a1b2c3d has no controller",
  });
  expect(LegionStateResponse.parse(refused).repositories.caches[0]?.state).toBe("refused");
  Object.assign(refused.repositories.caches[0] ?? {}, { state: "gone" });
  expect(LegionStateResponse.safeParse(refused).success).toBeFalse();
});

// The state's capability report (contract 16): the golden carries a decided row with the operator's
// reason and an open row with the legion.yaml line that records a decision, beside the present
// (codegraph among them: the image carries the tooling and a pod's launch loads it with extension
// discovery on; claude-code too, since one worker image carries the claude-envoy plugin and the
// pin a pod installs Claude Code from for a role whose worker_backends names it) and withheld
// rows, and the six live rows unchecked — no session has reported, so
// the deployment renders each as `unchecked` with its ruling (contract 21) — and the report is
// never absent from a state. `installed` left the status set at contract 25: it was codegraph's
// until LEGION-629, and no row has carried it since. Each deployment row carries `source`
// (contract 25): `configuration` (what legion.yaml alone decides) or `boot` (what boot measured);
// a live row carries none.
test("the state golden carries the deployment's capability report", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));

  const rows = Object.fromEntries(state.capabilities.map((row) => [row.name, row]));
  expect(rows.secrets).toEqual({
    name: "secrets",
    status: "decided",
    detail: "runtime.kubernetes.agent_secrets is not configured",
    decision:
      "pods are enrolled with the secrets broker once dispatch://LEGION-205 lands; until then no pod reads a secret",
    source: "configuration",
  });
  expect(rows["resource-limits"]).toEqual({
    name: "resource-limits",
    status: "open",
    detail:
      "no CPU and memory requests and limits under runtime.kubernetes.resources for the issue pod",
    configLine: 'capabilities.decided.resource-limits: "<reason>"',
    source: "configuration",
  });
  expect(rows.codegraph).toEqual({
    name: "codegraph",
    status: "present",
    detail: "checked by the daemon's probe of the worker image, which passed",
  });
  // A capability `name` is a plain non-empty string, so a row the daemon adds — claude-code at
  // dispatch://INFRA-220 — parses against the schema already published; the golden carries it.
  expect(rows["claude-code"]).toEqual({
    name: "claude-code",
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
  expect(statuses).not.toContain("installed");

  const deploymentRows = [
    rows.secrets,
    rows["model-fallback"],
    rows["resource-limits"],
    rows["pool-capacity"],
  ];
  for (const row of deploymentRows) {
    expect(["configuration", "boot"]).toContain(row.source ?? "");
  }

  const emptied = fixture("state-stage3.json") as { capabilities: unknown[] };
  expect(emptied.capabilities).toEqual([]);
});

test("a capability row's status is one the report renders, and the report cannot be dropped", () => {
  const unknownStatus = fixture("state.json") as { capabilities: { status: string }[] };
  unknownStatus.capabilities = [{ ...unknownStatus.capabilities[0], status: "missing" }];
  expect(LegionStateResponse.safeParse(unknownStatus).success).toBeFalse();

  // Contract 16's `live` promised a check to come; since contract 21 a live row is `present`,
  // `open` or `unchecked` from the sessions' reports, and a daemon still rendering `live` is one
  // the plugin was not built against.
  const live = fixture("state.json") as { capabilities: { status: string }[] };
  live.capabilities = [{ ...live.capabilities[0], status: "live" }];
  expect(LegionStateResponse.safeParse(live).success).toBeFalse();

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.capabilities;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();
});

// A session's own report (contract 21): `claims/ready` carries what the session measured of the
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

// The claims nothing will wake (contract 23): the golden names LEGION-210's failed implementer,
// whose issue is held, and a state with none carries an empty list rather than none.
test("the state golden names the claims nothing will wake, and the list cannot be dropped", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.strandedClaims).toEqual([
    { issue: "LEGION-210", role: "implementer", state: "failed" },
  ]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).strandedClaims).toEqual([]);

  const running = fixture("state.json") as { strandedClaims: unknown[] };
  running.strandedClaims = [{ issue: "LEGION-210", role: "implementer", state: "idle" }];
  expect(LegionStateResponse.safeParse(running).success).toBeFalse();

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.strandedClaims;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();
});

// The lifecycle rows held behind their issues' running ones (contract 28): the golden carries
// LEGION-208's supervise row 802 waiting behind row 801 for two and a half minutes, a state with none
// carries an empty list rather than none, a row's wait is never negative, and the list cannot be
// dropped: a plugin from before the field refuses the state, which is what the bump is for.
test("the state golden names the held lifecycle rows, and the list cannot be dropped", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.heldOutboxRows).toEqual([
    { id: 802, kind: "supervise", issue: "LEGION-208", waitedSeconds: 150, waitsOn: 801 },
  ]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).heldOutboxRows).toEqual([]);

  const negative = fixture("state.json") as { heldOutboxRows: unknown[] };
  negative.heldOutboxRows = [
    { id: 802, kind: "supervise", issue: "LEGION-208", waitedSeconds: -1, waitsOn: 801 },
  ];
  expect(LegionStateResponse.safeParse(negative).success).toBeFalse();

  const unnamed = fixture("state.json") as { heldOutboxRows: unknown[] };
  unnamed.heldOutboxRows = [
    { id: 802, kind: "supervise", issue: "LEGION-208", waitedSeconds: 150 },
  ];
  expect(LegionStateResponse.safeParse(unnamed).success).toBeFalse();

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.heldOutboxRows;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();
});

// The daemon's resolved legion.yaml (contract 35): the golden carries the file's `max_fix_attempts`
// and `worker_boot_timeout_seconds` and its GitHub Apps' ids and installations with no key, a
// Stage 3 state carries the defaults, the two typed keys are positive integers, the rest rides
// loosely, and the block cannot be dropped: a plugin from before the field refuses the state.
test("the state golden carries the daemon's settings with no credential, and the block cannot be dropped", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.settings.max_fix_attempts).toBe(5);
  expect(state.settings.worker_boot_timeout_seconds).toBe(180);
  const apps = state.settings.github_apps as { implement: Record<string, unknown> };
  expect(Object.keys(apps.implement).sort()).toEqual(["app_id", "installations"]);

  const stage3 = LegionStateResponse.parse(fixture("state-stage3.json"));
  expect(stage3.settings.max_fix_attempts).toBe(3);
  expect(stage3.settings.worker_boot_timeout_seconds).toBe(120);

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.settings;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();

  const zero = fixture("state.json") as { settings: Record<string, unknown> };
  zero.settings.max_fix_attempts = 0;
  expect(LegionStateResponse.safeParse(zero).success).toBeFalse();

  const typed = fixture("state.json") as { settings: Record<string, unknown> };
  typed.settings.max_fix_attempts = "5";
  expect(LegionStateResponse.safeParse(typed).success).toBeFalse();

  const text = readFileSync(path.join(fixtureDir, "state.json"), "utf8");
  expect(text).not.toMatch(/BEGIN .*PRIVATE KEY/);
  expect(text).not.toMatch(/postgres:\/\//);
  expect(text).not.toMatch(/:\/\/[^/@\s]+:[^/@\s]+@/);
});

// The lag of the daemon's durable consumers (contract 43): the golden carries a caught-up consumer,
// one whose info NATS did not answer and one stalled with messages waiting; a state with no
// workflow carries an empty list; the oldest and the last handled are null rather than absent, a
// negative count is refused, and the list cannot be dropped: a plugin from before the field
// refuses the state, which is what the bump is for.
test("the state golden carries each durable consumer's lag, and the list cannot be dropped", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.consumers).toEqual([
    {
      name: "legion-go-LEGION-dispatch",
      waiting: 0,
      oldestAt: null,
      oldestAgeSeconds: null,
      lastHandledAt: "2026-09-22T09:17:50Z",
      stalled: false,
    },
    {
      name: "legion-go-LEGION-dispatch-server",
      waiting: 0,
      oldestAt: null,
      oldestAgeSeconds: null,
      lastHandledAt: null,
      stalled: false,
      unavailable: "read the consumer's info: context deadline exceeded",
    },
    {
      name: "legion-go-LEGION-github",
      waiting: 412,
      oldestAt: "2026-09-22T09:05:34Z",
      oldestAgeSeconds: 754,
      lastHandledAt: "2026-09-22T09:16:41Z",
      stalled: true,
    },
  ]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).consumers).toEqual([]);

  const absent = fixture("state.json") as { consumers: Record<string, unknown>[] };
  delete absent.consumers[0]?.oldestAt;
  expect(LegionStateResponse.safeParse(absent).success).toBeFalse();

  const negative = fixture("state.json") as { consumers: Record<string, unknown>[] };
  negative.consumers = [{ ...negative.consumers[0], waiting: -1 }];
  expect(LegionStateResponse.safeParse(negative).success).toBeFalse();

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.consumers;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();
});

// Each served repository Dispatch's GitHub App is not known to be installed on (contract 43): the
// golden carries one with its reason, a state with none carries an empty list, an entry without
// its reason is refused, and the list cannot be dropped: a plugin from before the field refuses
// the state.
test("the state golden names each served repository without the webhook App, and the list cannot be dropped", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.repositories.withoutWebhookApp).toEqual([
    {
      repo: "zeta/tools",
      reason:
        "Dispatch's GitHub App is not installed on zeta/tools: GitHub sends Legion none of its events and nothing redelivers them; the pass heals its pull requests every 2 minutes",
    },
  ]);
  expect(
    LegionStateResponse.parse(fixture("state-stage3.json")).repositories.withoutWebhookApp
  ).toEqual([]);

  const reasonless = fixture("state.json") as {
    repositories: { withoutWebhookApp: Record<string, unknown>[] };
  };
  delete reasonless.repositories.withoutWebhookApp[0]?.reason;
  expect(LegionStateResponse.safeParse(reasonless).success).toBeFalse();

  const dropped = fixture("state.json") as { repositories: Record<string, unknown> };
  delete dropped.repositories.withoutWebhookApp;
  expect(LegionStateResponse.safeParse(dropped).success).toBeFalse();
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
