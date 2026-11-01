import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { controllerToken, legionControllerNoticeSubject } from "@legion/contracts";
import { deliverSubject } from "../consumer-delivery";
import {
  controllerNoticeRole,
  reachesPrivateStreamSubject,
  registrationTopics,
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
  // What the listener refuses the whole registration for (`bus.ReachesNoSession`).
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

/**
 * The listener's own table: `TestReachesPrivateStreamSubjectBoundsInterests` in
 * packages/envoy/internal/bus/stored_test.go, the file this mirror answers case for case. The
 * rows are read out of that Go source rather than retyped, so a case added there is run here on
 * the next test run instead of drifting out of a hand copy.
 */
const GO_TABLE = resolve(import.meta.dir, "../../../envoy/internal/bus/stored_test.go");
const GO_TABLE_TEST = "func TestReachesPrivateStreamSubjectBoundsInterests(t *testing.T) {";

interface GoCase {
  readonly sessionID: string;
  readonly topic: string;
  readonly roles: readonly string[];
  readonly want: boolean;
}

/** The rows of the Go table, each `{sessionID: "…", topic: "…", roles: …, want: …},`. */
function goTableCases(source: string): GoCase[] {
  const start = source.indexOf(GO_TABLE_TEST);
  if (start < 0) throw new Error(`${GO_TABLE} no longer holds ${GO_TABLE_TEST}`);
  // The table ends with the function: its own closing brace is the first one in column 0.
  const body = source.slice(start, source.indexOf("\n}", start));
  // The slices the rows name, such as `controller := []string{"legion-ACME-controller", …}`.
  const named = new Map<string, string[]>();
  for (const [, name, items] of body.matchAll(/(\w+) := \[\]string\{([^}]*)\}/g)) {
    named.set(
      name,
      [...items.matchAll(/"([^"]*)"/g)].map(([, item]) => item)
    );
  }
  const roleList = (written: string | undefined): readonly string[] => {
    if (written === undefined) return [];
    if (written.startsWith("[]string{")) {
      return [...written.matchAll(/"([^"]*)"/g)].map(([, item]) => item);
    }
    const slice = named.get(written);
    if (slice === undefined)
      throw new Error(`the Go table names a slice this test cannot read: ${written}`);
    return slice;
  };
  return [
    ...body.matchAll(
      /\{sessionID: "([^"]*)", topic: "([^"]*)"(?:, roles: (\[\]string\{[^}]*\}|\w+))?, want: (true|false)\}/g
    ),
  ].map(([, sessionID, topic, roles, want]) => ({
    sessionID: sessionID ?? "",
    topic: topic ?? "",
    roles: roleList(roles),
    want: want === "true",
  }));
}

test("a topic reaching a private stream subject is the listener's rule, case for case", () => {
  const cases = goTableCases(readFileSync(GO_TABLE, "utf8"));
  // The Go table held 35 rows when this was written: a parse that reads a handful of them, or
  // none, passes while covering nothing.
  expect(cases.length).toBeGreaterThanOrEqual(35);
  for (const { sessionID, topic, roles, want } of cases) {
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

describe("the topics a registration carries", () => {
  const OWN = "notifications.agent.ses-1";

  test("are the session's own subject and every topic the listener would store for it", () => {
    expect(
      registrationTopics(OWN, [
        "notifications.team.deploys",
        OWN,
        "notifications.dispatch.issue.DSP-1.>",
        "notifications.role.reviewer",
        "team.deploys",
        "notifications.team.deploys",
      ])
    ).toEqual([
      OWN,
      "notifications.team.deploys",
      "notifications.dispatch.issue.DSP-1.>",
      // A role lane is its holder's to register (POST /v1/roles/set) and the listener stores the
      // interest, and a topic outside `notifications.` is no concern of the stream's rules.
      "notifications.role.reviewer",
      "team.deploys",
    ]);
  });

  test("leave out a topic nothing would be delivered on, which would be refused 400", () => {
    // `notifications.team` is the bare base an earlier build expanded `notifications.team.>` to,
    // and the one a stored row or a transcript brings back (INFRA-771 review).
    expect(
      registrationTopics(OWN, ["notifications.team", "notifications.lxspec.probe.all"])
    ).toEqual([OWN]);
  });

  test("leave out a private stream subject that is not this session's, the role's lane apart", () => {
    const lane = legionControllerNoticeSubject("acme");
    expect(
      registrationTopics(OWN, ["notifications.agent.ses-other", lane, "notifications.agent.*"])
    ).toEqual([OWN]);
    expect(registrationTopics(OWN, [lane], [controllerToken("acme")])).toEqual([OWN, lane]);
  });
});
