import { expect, test } from "bun:test";
import { LegionDaemonApi } from "./legion-daemon-api";

test("requires a transcript-derived agent id to establish a root architect capability", () => {
  const processStarted = {
    tree: "acme/widgets#1",
    generation: 1,
    rootSessionId: "ses_root",
    bootToken: "boot-capability",
    ompSessionFile: "/tmp/root.jsonl",
    pluginVersion: "1.46.0",
  };

  expect(LegionDaemonApi.ProcessStarted.request.safeParse(processStarted).success).toBeFalse();
  expect(
    LegionDaemonApi.ProcessStarted.request.safeParse({
      ...processStarted,
      agentId: "root",
    }).success
  ).toBeTrue();
});

test("requires a transcript-derived agent id to establish a worker capability", () => {
  const workerStarted = {
    tree: "acme/widgets#1",
    issue: "acme/widgets#42",
    role: "tester",
    bootToken: "boot-capability",
    sessionId: "ses_worker",
    ompSessionFile: "/tmp/worker.jsonl",
  };

  expect(LegionDaemonApi.WorkerStarted.request.safeParse(workerStarted).success).toBeFalse();
  expect(
    LegionDaemonApi.WorkerStarted.request.safeParse({
      ...workerStarted,
      agentId: "worker",
    }).success
  ).toBeFalse();
  expect(
    LegionDaemonApi.WorkerStarted.request.safeParse({
      ...workerStarted,
      agentId: "worker",
      pluginVersion: "1.49.0",
    }).success
  ).toBeTrue();
});

test("requires a grant and bot login to redeem a GitHub App token", () => {
  expect(LegionDaemonApi.GitHubToken.request.safeParse({}).success).toBeFalse();
  expect(
    LegionDaemonApi.GitHubToken.response.safeParse({ token: "app-token" }).success
  ).toBeFalse();
  expect(
    LegionDaemonApi.GitHubToken.response.safeParse({
      token: "human-token",
      appLogin: "sjawhar",
    }).success
  ).toBeFalse();
  expect(
    LegionDaemonApi.GitHubToken.response.safeParse({
      token: "app-token",
      appLogin: "legion-implementer[bot]",
    }).success
  ).toBeTrue();
});

test("ControllerReady.request requires a plugin version and accepts an optional OMP session file", () => {
  const claim = { secret: "controller-secret", sessionId: "ses_controller", pluginVersion: "1.46.0" };
  expect(LegionDaemonApi.ControllerReady.request.safeParse(claim).success).toBeTrue();
  expect(
    LegionDaemonApi.ControllerReady.request.safeParse({
      ...claim,
      ompSessionFile: "/tmp/controller.jsonl",
    }).success
  ).toBeTrue();
  expect(
    LegionDaemonApi.ControllerReady.request.safeParse({ ...claim, ompSessionFile: "" }).success
  ).toBeFalse();
  expect(
    LegionDaemonApi.ControllerReady.request.safeParse({
      secret: "controller-secret",
      sessionId: "ses_controller",
    }).success
  ).toBeFalse();
});

test("Grant.request accepts the worker form and the controller form and rejects a half form", () => {
  expect(
    LegionDaemonApi.Grant.request.safeParse({ sessionId: "ses_controller", secret: "s" }).success
  ).toBeTrue();
  expect(
    LegionDaemonApi.Grant.request.safeParse({
      tree: "WIDGETS-1",
      issue: "WIDGETS-2",
      sessionId: "ses_worker",
      secret: "s",
    }).success
  ).toBeTrue();
  for (const half of [{ tree: "WIDGETS-1" }, { issue: "WIDGETS-2" }]) {
    expect(
      LegionDaemonApi.Grant.request.safeParse({ ...half, sessionId: "ses_worker", secret: "s" })
        .success
    ).toBeFalse();
  }
});

test("GitCredential.request rejects merge intent: the guardrail is /gh-token's alone", () => {
  expect(LegionDaemonApi.GitCredential.request.safeParse({ grantId: "g" }).success).toBeTrue();
  expect(
    LegionDaemonApi.GitCredential.request.safeParse({ grantId: "g", merge: true }).success
  ).toBeFalse();
});

test("GitHubToken.request is exactly { grantId }: merge intent is not a Legion concept", () => {
  expect(LegionDaemonApi.GitHubToken.request.safeParse({ grantId: "g" }).success).toBeTrue();
  expect(
    LegionDaemonApi.GitHubToken.request.safeParse({ grantId: "g", merge: true }).success
  ).toBeFalse();
});

test("State.response accepts the redacted projection shape but rejects a leaked secret/hash field", () => {
  const redacted = {
    project: "acme/widgets",
    version: 21,
    issues: {
      "WIDGETS-1": {
        key: "WIDGETS-1",
        title: "Root",
        status: "in_progress",
        children: ["WIDGETS-2"],
        lastAppliedSeq: 4,
      },
    },
    trees: {
      "WIDGETS-1": {
        status: "active",
        generation: 1,
        launchFailures: 0,
        readyConfirmedAt: 1700000000000,
        locator: {
          runtime: "tmux",
          tmuxSession: "legion-acme",
          tmuxWindowId: "@1",
          tmuxPaneId: "%1",
        },
      },
    },
    admission: { cap: 2, active: ["WIDGETS-1"], queue: [] },
    gates: { "WIDGETS-1": { artifactId: "art-1", latestVersion: 3, approvedVersion: 3 } },
    controllerLocator: {
      runtime: "tmux",
      tmuxSession: "legion-acme",
      tmuxWindowId: "@0",
      tmuxPaneId: "%0",
      ompSessionFile: "/tmp/controller.jsonl",
    },
    roles: {
      "legion:acme:controller": { role: "controller", sessionId: "ses_controller" },
      "legion:acme:WIDGETS-1:implementer": {
        role: "implementer",
        issue: "WIDGETS-1",
        generation: 1,
        sessionId: "ses_implementer",
        readyConfirmedAt: 1700000000000,
        launchFailures: 0,
        locator: {
          runtime: "tmux",
          tmuxSession: "legion-acme",
          tmuxWindowId: "@2",
          tmuxPaneId: "%2",
        },
      },
    },
    controllerPendingNotices: 0,
    pendingStatusWrites: ["WIDGETS-2"],
    workerAdmission: {
      queue: [
        {
          roleToken: "legion-acme-WIDGETS-2-tester",
          issue: "WIDGETS-2",
          role: "tester",
          kind: "assignment",
          queuedAt: "2026-09-13T17:00:00.000Z",
        },
        { roleToken: "legion-acme-WIDGETS-3-planner", issue: "WIDGETS-3", role: "planner" },
      ],
    },
  };

  expect(LegionDaemonApi.State.response.safeParse(redacted).success).toBeTrue();
  expect(
    LegionDaemonApi.State.response.safeParse({
      ...redacted,
      controllerLocator: undefined,
      trees: {
        ...redacted.trees,
        "WIDGETS-1": {
          ...redacted.trees["WIDGETS-1"],
          locator: {
            runtime: "kubernetes",
            namespace: "legion",
            podName: "legion-widgets-1-architect-g1",
            podUid: "uid-1",
            pvcName: "legion-widgets-1",
            ompSessionFile: "/legion/sessions/architect/session.jsonl",
          },
        },
      },
    }).success
  ).toBeTrue();

  for (const leak of [
    { ...redacted, controllerCapabilityHash: "deadbeef" },
    {
      ...redacted,
      roles: {
        ...redacted.roles,
        x: { ...redacted.roles["legion:acme:WIDGETS-1:implementer"], bootTokenHash: "deadbeef" },
      },
    },
    {
      ...redacted,
      trees: {
        ...redacted.trees,
        "WIDGETS-1": {
          ...redacted.trees["WIDGETS-1"],
          locator: { ...redacted.trees["WIDGETS-1"].locator, socketPath: "/tmp/x.sock" },
        },
      },
    },
    {
      ...redacted,
      trees: {
        ...redacted.trees,
        "WIDGETS-1": {
          ...redacted.trees["WIDGETS-1"],
          locator: { tmuxSession: "legion-acme", tmuxWindowId: "@1", tmuxPaneId: "%1" },
        },
      },
    },
    {
      ...redacted,
      controllerLocator: { ...redacted.controllerLocator, socketPath: "/tmp/c.sock" },
    },
  ]) {
    expect(LegionDaemonApi.State.response.safeParse(leak).success).toBeFalse();
  }
});

test("State.response accepts the external controller record and rejects it under trees", () => {
  const external = { runtime: "kubernetes", external: true, sessionId: "ses_op", registeredAt: 1 };
  const base = {
    project: "acme/widgets",
    version: 33,
    issues: {},
    trees: {},
    admission: { cap: 1, active: [], queue: [] },
    gates: {},
    roles: {},
    controllerPendingNotices: 0,
    pendingStatusWrites: [],
    workerAdmission: { queue: [] },
  };
  expect(
    LegionDaemonApi.State.response.safeParse({ ...base, controllerLocator: external }).success
  ).toBeTrue();
  expect(
    LegionDaemonApi.State.response.safeParse({
      ...base,
      trees: {
        "WIDGETS-1": { status: "active", generation: 1, launchFailures: 0, locator: external },
      },
    }).success
  ).toBeFalse();
  // A pod record that merely claims `external` is neither shape.
  expect(
    LegionDaemonApi.State.response.safeParse({
      ...base,
      controllerLocator: {
        runtime: "kubernetes",
        namespace: "legion",
        podName: "p",
        podUid: "u",
        pvcName: "v",
        external: true,
      },
    }).success
  ).toBeFalse();
});

test("ControllerSecret.response requires a non-empty secret and the request is empty", () => {
  expect(LegionDaemonApi.ControllerSecret.response.safeParse({ secret: "" }).success).toBeFalse();
  expect(LegionDaemonApi.ControllerSecret.response.safeParse({ secret: "s" }).success).toBeTrue();
  expect(LegionDaemonApi.ControllerSecret.request.safeParse({}).success).toBeTrue();
  // The operator token travels as a bearer header, never in the body.
  expect(LegionDaemonApi.ControllerSecret.request.safeParse({ token: "x" }).success).toBeFalse();
});

test("the state response refuses a worker-queue entry that carries the task text", () => {
  const queued = {
    roleToken: "legion-acme-WIDGETS-2-tester",
    issue: "WIDGETS-2",
    role: "tester",
    kind: "assignment",
    queuedAt: "2026-09-13T17:00:00.000Z",
  };
  const base = {
    project: "acme/widgets",
    version: 29,
    issues: {},
    trees: {},
    admission: { cap: 2, active: [], queue: [] },
    gates: {},
    roles: {},
    controllerPendingNotices: 0,
    pendingStatusWrites: [],
  };

  expect(
    LegionDaemonApi.State.response.safeParse({ ...base, workerAdmission: { queue: [queued] } })
      .success
  ).toBeTrue();
  expect(
    LegionDaemonApi.State.response.safeParse({
      ...base,
      workerAdmission: { queue: [{ ...queued, task: "plan #1" }] },
    }).success
  ).toBeFalse();
});

test("the state response requires queue kind and queuedAt together", () => {
  const queued = {
    roleToken: "legion-acme-WIDGETS-2-tester",
    issue: "WIDGETS-2",
    role: "tester",
    kind: "assignment",
    queuedAt: "2026-09-13T17:00:00.000Z",
  };
  const { kind: _kind, ...withoutKind } = queued;
  const { queuedAt: _queuedAt, ...withoutQueuedAt } = queued;
  const base = {
    project: "acme/widgets",
    version: 29,
    issues: {},
    trees: {},
    admission: { cap: 2, active: [], queue: [] },
    gates: {},
    roles: {},
    controllerPendingNotices: 0,
    pendingStatusWrites: [],
  };

  for (const partial of [withoutKind, withoutQueuedAt]) {
    expect(
      LegionDaemonApi.State.response.safeParse({
        ...base,
        workerAdmission: { queue: [partial] },
      }).success
    ).toBeFalse();
  }
});

test("SpawnWorker.request requires a UUID requestId", () => {
  const body = {
    tree: "WIDGETS-1",
    sessionId: "ses_architect",
    secret: "root-secret",
    issue: "WIDGETS-2",
    role: "planner",
    task: "plan #1",
  };

  for (const missing of [body, { ...body, requestId: "not-a-uuid" }]) {
    const parsed = LegionDaemonApi.SpawnWorker.request.safeParse(missing);
    expect(parsed.success).toBeFalse();
    expect(parsed.error?.issues.map((issue) => issue.path)).toEqual([["requestId"]]);
  }
  expect(
    LegionDaemonApi.SpawnWorker.request.safeParse({
      ...body,
      requestId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
    }).success
  ).toBeTrue();
});

test("GatesRegister.request names the spec document by its id and version, never an ask id or a slug", () => {
  const capability = { tree: "WIDGETS-1", sessionId: "ses_architect", secret: "root-secret" };
  const documentId = "4e0aca36-77b3-43bd-96cf-d58890ae64e4";
  expect(
    LegionDaemonApi.GatesRegister.request.safeParse({
      ...capability,
      issue: "WIDGETS-1",
      artifactId: documentId,
      version: 2,
    }).success
  ).toBeTrue();
  const askId = LegionDaemonApi.GatesRegister.request.safeParse({
    ...capability,
    issue: "WIDGETS-1",
    askId: "ask-1",
  });
  expect(askId.success).toBeFalse();
  // The slug an architect typed into dispatch_request_approval is not the document id the
  // approval events carry; registering it would create a gate no event can open.
  const slug = LegionDaemonApi.GatesRegister.request.safeParse({
    ...capability,
    issue: "WIDGETS-1",
    artifactId: "spec",
    version: 2,
  });
  expect(slug.success).toBeFalse();
  expect(slug.success ? [] : slug.error.issues.map((issue) => issue.path.join("."))).toEqual([
    "artifactId",
  ]);
  const missingVersion = LegionDaemonApi.GatesRegister.request.safeParse({
    ...capability,
    issue: "WIDGETS-1",
    artifactId: documentId,
  });
  expect(missingVersion.success).toBeFalse();
  expect(
    LegionDaemonApi.GatesRegister.request.safeParse({
      ...capability,
      issue: "WIDGETS-1",
      artifactId: documentId,
      version: 0,
    }).success
  ).toBeFalse();
});
