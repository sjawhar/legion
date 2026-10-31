import { describe, expect, test } from "bun:test";
import { controllerToken, legionControllerNoticeSubject } from "@legion/contracts";
import { deliverSubject } from "../consumer-delivery";
import {
  controllerNoticeRole,
  reachesPrivateStreamSubject,
  rememberedTopics,
  replayFollowedSubjects,
  streamDeliversNothing,
  streamStores,
} from "../stream-topics";

test("the stream stores agent, producer and team topics, wildcards read literally", () => {
  expect(streamStores("notifications.agent.ses_a")).toBe(true);
  expect(streamStores("notifications.dispatch.issue.DSP-7.>")).toBe(true);
  expect(streamStores("notifications.github.acme.repo.issue.3")).toBe(true);
  expect(streamStores("notifications.team.deploy")).toBe(true);
  expect(streamStores("notifications.envoy.exceptions.notifications.agent.ses_a")).toBe(true);
  expect(streamStores("notifications.envoy.exceptions.notifications.team.deploy")).toBe(true);
});

test("an uncaptured topic, bare prefixes and the relay subject are not stored", () => {
  expect(streamStores("notifications.lxspec.probe.all")).toBe(false);
  expect(streamStores("notifications.teamx.deploy")).toBe(false);
  expect(streamStores("notifications.dispatch.")).toBe(false);
  expect(streamStores("notifications.>")).toBe(false);
  expect(streamStores(deliverSubject("ses_a"))).toBe(false);
});

test("a topic nothing would be delivered on is one no stream subject captures, a role lane apart", () => {
  // What the listener refuses the whole registration for (`bus.UsesCoreTransport`).
  expect(streamDeliversNothing("notifications.team")).toBe(true);
  expect(streamDeliversNothing("notifications.github")).toBe(true);
  expect(streamDeliversNothing("notifications.lxspec.probe.all")).toBe(true);
  expect(streamDeliversNothing("notifications.envoy.deliver.ses_a")).toBe(true);
  // A role lane travels over core NATS and its holder registers it.
  expect(streamDeliversNothing("notifications.role.legion-omp-controller")).toBe(false);
  expect(streamDeliversNothing("notifications.team.deploys")).toBe(false);
  expect(streamDeliversNothing("notifications.agent.ses_a")).toBe(false);
  // Only a notifications. topic is the listener's to refuse by name.
  expect(streamDeliversNothing("team.deploys")).toBe(false);
});

describe("a controller notice lane", () => {
  test("names the role token its project's controller holds, as contracts mints it", () => {
    expect(controllerNoticeRole(legionControllerNoticeSubject("acme"))).toBe(
      controllerToken("acme")
    );
    expect(controllerNoticeRole("notifications.legion.ACME.controller")).toBe(
      "legion-ACME-controller"
    );
  });

  test("is one literal subject: no wildcard, no empty project, no issue notice", () => {
    expect(controllerNoticeRole("notifications.legion.*.controller")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion.>")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion..controller")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion.controller")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion.acme.LEGION-7")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion.acme.sub.controller")).toBeUndefined();
    expect(controllerNoticeRole("notifications.agent.ses_a")).toBeUndefined();
  });
});

// Every case of the listener's own table, TestReachesPrivateStreamSubjectBoundsInterests in
// packages/envoy/internal/bus/stored_test.go, read against the mirror here.
test("a topic reaching a private stream subject is the listener's rule, case for case", () => {
  const controller = ["legion-ACME-controller", "reviewer"];
  const cases: readonly [string, string, readonly string[], boolean][] = [
    ["ses_a", "notifications.agent.ses_a", [], false],
    ["ses_a", "notifications.agent.ses_b", [], true],
    ["ses_a", "notifications.agent.*", [], true],
    ["ses_a", "notifications.agent.>", [], true],
    ["ses_a", "notifications.agent.ses_a.>", [], true],
    ["ses_a", "notifications.>", [], true],
    ["ses_a", "notifications.*.ses_a", [], true],
    ["ses_a", "notifications.*.*.x", [], true],
    ["ses_a", ">", [], true],
    [">", "notifications.agent.>", [], true],
    ["*", "notifications.agent.*", [], true],
    ["ses_a", "notifications.envoy.exceptions.notifications.agent.ses_a", [], true],
    ["ses_a", "notifications.envoy.exceptions.notifications.agent.ses_b", [], true],
    ["ses_a", "notifications.envoy.exceptions.notifications.agent.>", [], true],
    ["ses_a", "notifications.envoy.exceptions.notifications.team.deploys", [], true],
    ["ses_a", "notifications.envoy.exceptions.notifications.team.>", [], true],
    ["ses_a", "notifications.envoy.exceptions.>", [], true],
    ["ses_a", "notifications.envoy.>", [], true],
    ["ses_a", "notifications.legion.ACME.controller", [], true],
    ["ses_a", "notifications.legion.>", [], true],
    ["ses_a", "notifications.*.ACME.controller", [], true],
    ["ses_a", "notifications.legion.ACME.controller", controller, false],
    ["ses_a", "notifications.legion.OTHER.controller", controller, true],
    ["ses_a", "notifications.legion.>", controller, true],
    ["ses_a", "notifications.legion.*.controller", controller, true],
    ["ses_a", "notifications.agent.ses_b", controller, true],
    ["ses_a", "notifications.envoy.exceptions.notifications.agent.ses_a", controller, true],
    ["ses_a", "notifications.legion.ACME.controller", ["legion-OTHER-controller"], true],
    ["ses_a", "notifications.agent", [], false],
    ["ses_a", "notifications.*", [], false],
    ["ses_a", "notifications.legion", [], false],
    ["ses_a", "notifications.dispatch.issue.KEY-1.>", [], false],
    ["ses_a", "notifications.github.acme.widgets.pr.7.>", [], false],
    ["ses_a", "notifications.team.x.>", [], false],
    ["ses_a", "notifications.role.reviewer", [], false],
    ["ses_a", "notifications.envoy.exceptions.notifications.role.reviewer", [], false],
    ["ses_a", "notifications.exceptions.agent.ses_b", [], false],
  ];
  for (const [sessionID, topic, roles, want] of cases) {
    expect({
      topic,
      roles,
      reaches: reachesPrivateStreamSubject(`notifications.agent.${sessionID}`, topic, roles),
    }).toEqual({ topic, roles, reaches: want });
  }
});

describe("the subjects a restore follows again", () => {
  const ISSUE = "notifications.dispatch.issue.DSP-1.>";

  test("are the session's own agent subject and its topics, never a role's", () => {
    const followed = replayFollowedSubjects(
      "notifications.agent.ses-1",
      [
        ISSUE,
        "notifications.role.reviewer",
        "notifications.agent.ses-parent",
        "notifications.agent.ses-1",
        "notifications.legion.acme.controller",
        ISSUE,
        "team.deploys",
      ],
      (topic) => topic === "notifications.legion.acme.controller"
    );

    expect(followed).toEqual(["notifications.agent.ses-1", ISSUE, "team.deploys"]);
    expect(replayFollowedSubjects("notifications.agent.ses-1", [])).toEqual([
      "notifications.agent.ses-1",
    ]);
  });

  test("leave out a controller lane even for its holder: the role's claim follows it again", () => {
    const lane = legionControllerNoticeSubject("acme");
    expect(replayFollowedSubjects("notifications.agent.ses-1", [lane])).toEqual([
      "notifications.agent.ses-1",
    ]);
  });

  test("are the persisted topics but agent subjects and role lanes", () => {
    expect(
      rememberedTopics([
        "notifications.agent.ses-1",
        "notifications.agent.ses-parent",
        "notifications.role.reviewer",
        ISSUE,
        "team.deploys",
      ])
    ).toEqual([ISSUE, "team.deploys"]);
    expect(rememberedTopics(undefined)).toEqual([]);
    expect(rememberedTopics([])).toEqual([]);
  });
});
