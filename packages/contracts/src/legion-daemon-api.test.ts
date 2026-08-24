import { expect, test } from "bun:test";
import { LegionDaemonApi } from "./legion-daemon-api";

test("requires a spawn token to activate a Legion phase", () => {
  expect(
    LegionDaemonApi.Phase.request.safeParse({
      tree: "acme/widgets#1",
      issue: "acme/widgets#42",
      phase: "implementer",
      sessionId: "ses_worker",
    }).success
  ).toBeFalse();

  expect(
    LegionDaemonApi.Phase.request.safeParse({
      tree: "acme/widgets#1",
      issue: "acme/widgets#42",
      phase: "implementer",
      sessionId: "ses_worker",
      spawnToken: "spawn-capability",
    }).success
  ).toBeTrue();
});
