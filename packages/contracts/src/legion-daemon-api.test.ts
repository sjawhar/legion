import { expect, test } from "bun:test";
import { LegionDaemonApi } from "./legion-daemon-api";

test("requires a transcript-derived agent id to establish a root architect capability", () => {
  const processStarted = {
    tree: "acme/widgets#1",
    generation: 1,
    rootSessionId: "ses_root",
    bootToken: "boot-capability",
    ompSessionFile: "/tmp/root.jsonl",
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
    controllerLocator: { runtime: "tmux", tmuxSession: "legion-acme", tmuxWindowId: "@0" },
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
  ]) {
    expect(LegionDaemonApi.State.response.safeParse(leak).success).toBeFalse();
  }
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
