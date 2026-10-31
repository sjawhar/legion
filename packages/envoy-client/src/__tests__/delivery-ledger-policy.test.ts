import { describe, expect, test } from "bun:test";
import { decode } from "@toon-format/toon";
import { type ConsumerGapNotice, gapNoticeText } from "../consumer-delivery";
import {
  closeReplayedRemoval,
  reachesPrivateStreamSubject,
  rememberedTopics,
  replayFollowedSubjects,
} from "../delivery-ledger";

const ISSUE = "notifications.dispatch.issue.DSP-1.>";
const OTHER_ISSUE = "notifications.dispatch.issue.DSP-2.>";

describe("the subjects a session follows", () => {
  test("are its own agent subject and its topics, never a role's or another session's agent subject", () => {
    const followed = replayFollowedSubjects(
      "notifications.agent.ses-1",
      [
        ISSUE,
        "notifications.role.reviewer",
        "notifications.agent.ses-parent",
        "notifications.agent.ses-1",
        "notifications.legion.notice.controller",
        ISSUE,
        "team.deploys",
      ],
      (topic) => topic === "notifications.legion.notice.controller"
    );

    expect(followed).toEqual(["notifications.agent.ses-1", ISSUE, "team.deploys"]);
    expect(replayFollowedSubjects("notifications.agent.ses-1", [])).toEqual([
      "notifications.agent.ses-1",
    ]);
  });

  // The listener's rule, packages/envoy/internal/bus (TestReachesPrivateStreamSubjectBoundsInterests).
  test("leave out every topic reaching a private stream subject but the session's own agent subject, as the listener's rule does", () => {
    const own = "notifications.agent.ses_a";
    const reaching = [
      "notifications.agent.ses_b",
      "notifications.agent.*",
      "notifications.agent.>",
      "notifications.agent.ses_a.>",
      "notifications.>",
      "notifications.*.ses_a",
      "notifications.*.*.x",
      ">",
      "notifications.envoy.exceptions.notifications.agent.ses_a",
      "notifications.envoy.exceptions.notifications.agent.ses_b",
      "notifications.envoy.exceptions.notifications.agent.>",
      "notifications.envoy.exceptions.>",
      "notifications.envoy.>",
      "notifications.legion.ACME.controller",
      "notifications.legion.>",
      "notifications.*.ACME.controller",
    ];
    const kept = [
      ISSUE,
      "notifications.agent",
      "notifications.*",
      "notifications.legion",
      "notifications.github.acme.widgets.pr.7.>",
      "notifications.team.x.>",
      "notifications.envoy.exceptions.notifications.role.reviewer",
      "notifications.exceptions.agent.ses_b",
    ];
    expect(replayFollowedSubjects(own, [...reaching, ...kept])).toEqual([own, ...kept]);
    for (const topic of reaching) expect(reachesPrivateStreamSubject(own, topic)).toBe(true);
    for (const topic of [own, ...kept, "notifications.role.reviewer"]) {
      expect(reachesPrivateStreamSubject(own, topic)).toBe(false);
    }
    // A session id holding a wildcard makes its "own" agent subject no session's alone.
    expect(reachesPrivateStreamSubject("notifications.agent.>", "notifications.agent.>")).toBe(
      true
    );
    expect(reachesPrivateStreamSubject("notifications.agent.*", "notifications.agent.*")).toBe(
      true
    );
  });
});

describe("the topics a restore follows again", () => {
  test("are the persisted state's topics but agent subjects and role lanes", () => {
    expect(
      rememberedTopics({
        topics: [
          "notifications.agent.ses-1",
          "notifications.agent.ses-parent",
          "notifications.role.reviewer",
          ISSUE,
          "team.deploys",
        ],
      })
    ).toEqual([ISSUE, "team.deploys"]);
    expect(rememberedTopics(undefined)).toEqual([]);
    // A state an earlier build wrote held cursors and no topic list.
    expect(rememberedTopics({ topics: [] })).toEqual([]);
  });
});

describe("a stored subscription removal", () => {
  /** A Dispatch `subscription.removed` naming `session`, as the stream stores it. */
  const removal = (session: string, topics: readonly string[]) =>
    JSON.stringify({
      source: "dispatch",
      payload: JSON.stringify({
        issue_key: "DSP-1",
        type: "subscription.removed",
        actor: { kind: "user", id: "alice" },
        payload: { session_id: session, by: { kind: "user", id: "alice" }, topics },
      }),
    });

  /** A host following each topic the frame predates (true) or does not (false). */
  const host = (followed: Record<string, boolean>) => {
    const open = new Set(Object.keys(followed));
    const calls: string[] = [];
    let unregistered = false;
    return {
      open,
      calls,
      unregisteredBeforeReturn: () => unregistered,
      removalHost: {
        sessionID: "ses-1",
        followedBefore: (topic: string) => followed[topic] === true,
        close: (topic: string) => open.delete(topic),
        unregister: async (topics: readonly string[]) => {
          calls.push(`unregister ${topics.join(" ")}`);
          await Promise.resolve();
          unregistered = true;
        },
        followedChanged: async () => {
          calls.push("followedChanged");
        },
      },
    };
  };

  test("closes only a topic followed before it was stored, and takes it out of the registry", async () => {
    const session = host({ [ISSUE]: true, [OTHER_ISSUE]: false });

    const closed = await closeReplayedRemoval(
      { data: removal("ses-1", [ISSUE, OTHER_ISSUE, "notifications.dispatch.issue.DSP-3.>"]) },
      session.removalHost
    );

    expect(closed).toEqual([ISSUE]);
    expect([...session.open]).toEqual([OTHER_ISSUE]);
    expect(session.calls).toEqual([`unregister ${ISSUE}`, "followedChanged"]);
    expect(session.unregisteredBeforeReturn()).toBe(true);
  });

  test("closes nothing for another session's removal, or one stored before the follow", async () => {
    const session = host({ [ISSUE]: true });
    const later = host({ [OTHER_ISSUE]: false });

    expect(
      await closeReplayedRemoval({ data: removal("ses-2", [ISSUE]) }, session.removalHost)
    ).toEqual([]);
    expect(
      await closeReplayedRemoval({ data: removal("ses-1", [OTHER_ISSUE]) }, later.removalHost)
    ).toEqual([]);
    expect(
      await closeReplayedRemoval(
        { data: JSON.stringify({ source: "dispatch" }) },
        session.removalHost
      )
    ).toEqual([]);
    expect([...session.open]).toEqual([ISSUE]);
    expect([...later.open]).toEqual([OTHER_ISSUE]);
    expect(session.calls).toEqual([]);
  });
});

describe("a gap notice", () => {
  const card = (notice: ConsumerGapNotice) => decode(gapNoticeText(notice));

  test("is a card for the session, named by its kind", () => {
    expect(card({ kind: "stream_recreated", message: "recreated" })).toEqual({
      envoy: { notice: "stream recreated", detail: "recreated" },
    });
    expect(card({ kind: "retention_gap", message: "too old" })).toEqual({
      envoy: { notice: "messages may be missing", detail: "too old" },
    });
  });
});
