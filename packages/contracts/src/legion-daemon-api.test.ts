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
        locator: { tmuxSession: "legion-acme", tmuxWindowId: "@1", tmuxPaneId: "%1" },
      },
    },
    admission: { cap: 2, active: ["WIDGETS-1"], queue: [] },
    gates: { "WIDGETS-1": { designAskId: "ask-1", designApproved: "ask-1" } },
    controllerLocator: { tmuxSession: "legion-acme", tmuxWindowId: "@0" },
    roles: {
      "legion:acme:controller": { role: "controller", sessionId: "ses_controller" },
      "legion:acme:WIDGETS-1:implementer": {
        role: "implementer",
        issue: "WIDGETS-1",
        generation: 1,
        sessionId: "ses_implementer",
        readyConfirmedAt: 1700000000000,
        launchFailures: 0,
        locator: { tmuxSession: "legion-acme", tmuxWindowId: "@2", tmuxPaneId: "%2" },
      },
    },
    controllerPendingNotices: 0,
    pendingStatusWrites: ["WIDGETS-2"],
  };

  expect(LegionDaemonApi.State.response.safeParse(redacted).success).toBeTrue();

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
  ]) {
    expect(LegionDaemonApi.State.response.safeParse(leak).success).toBeFalse();
  }
});
