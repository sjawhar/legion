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

test("ControllerReady.request accepts an optional OMP session file", () => {
  const claim = { secret: "controller-secret", sessionId: "ses_controller" };
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
});

test("Grant.request accepts the controller form without tree or issue", () => {
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
});

test("GitHubToken.request carries merge intent only as the literal true", () => {
  expect(
    LegionDaemonApi.GitHubToken.request.safeParse({ grantId: "g", merge: true }).success
  ).toBeTrue();
  expect(
    LegionDaemonApi.GitHubToken.request.safeParse({ grantId: "g", merge: false }).success
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
    gates: { "WIDGETS-1": { designAskId: "ask-1", designApproved: "ask-1" } },
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
