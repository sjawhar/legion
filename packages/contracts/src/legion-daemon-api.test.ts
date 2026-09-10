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
