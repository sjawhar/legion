import { describe, expect, test } from "bun:test";
import { decode } from "@toon-format/toon";
import {
  closeReplayedRemoval,
  coversAnotherAgentSubject,
  type ReplayNotice,
  rememberedTopics,
  replayFollowedSubjects,
  routeReplayNotice,
} from "../replay";

const ISSUE = "notifications.dispatch.issue.DSP-1.>";
const OTHER_ISSUE = "notifications.dispatch.issue.DSP-2.>";

describe("the subjects a session follows for replay", () => {
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

  test("leave out every wildcard reaching another session's agent subject, as the listener does", () => {
    const own = "notifications.agent.ses-1";
    const wide = [
      "notifications.agent.*",
      "notifications.agent.>",
      "notifications.>",
      "notifications.*.ses-1",
      "notifications.*.*.x",
      ">",
    ];
    expect(replayFollowedSubjects(own, [...wide, ISSUE, "notifications.*"])).toEqual([
      own,
      ISSUE,
      "notifications.*",
    ]);
    for (const topic of [...wide, "notifications.agent.ses-parent"]) {
      expect(coversAnotherAgentSubject(own, topic)).toBe(true);
    }
    for (const topic of [
      own,
      ISSUE,
      "notifications.agent",
      "notifications.*",
      "notifications.role.x",
    ]) {
      expect(coversAnotherAgentSubject(own, topic)).toBe(false);
    }
  });
});

describe("the topics a restore follows again", () => {
  test("are the persisted state's subjects but agent subjects and role lanes", () => {
    expect(
      rememberedTopics({
        stream_created: "2026-10-26T00:00:00Z",
        subjects: {
          "notifications.agent.ses-1": { since: 1, after: 4 },
          "notifications.agent.ses-parent": {},
          "notifications.role.reviewer": {},
          [ISSUE]: { since: 2, after: 4 },
          "team.deploys": {},
        },
      })
    ).toEqual([ISSUE, "team.deploys"]);
    expect(rememberedTopics(undefined)).toEqual([]);
  });
});

describe("a replayed subscription removal", () => {
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

  /** A host following `followed`, each topic since the stream sequence given. */
  const host = (followed: Record<string, number>) => {
    const open = new Set(Object.keys(followed));
    const calls: string[] = [];
    let unregistered = false;
    return {
      open,
      calls,
      unregisteredBeforeReturn: () => unregistered,
      removalHost: {
        sessionID: "ses-1",
        engine: {
          followedBefore: (topic: string, seq: number) => {
            const since = followed[topic];
            return since !== undefined && seq > since;
          },
        },
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
    const session = host({ [ISSUE]: 10, [OTHER_ISSUE]: 50 });

    const closed = await closeReplayedRemoval(
      {
        data: removal("ses-1", [ISSUE, OTHER_ISSUE, "notifications.dispatch.issue.DSP-3.>"]),
        seq: 20,
      },
      session.removalHost
    );

    expect(closed).toEqual([ISSUE]);
    expect([...session.open]).toEqual([OTHER_ISSUE]);
    expect(session.calls).toEqual([`unregister ${ISSUE}`, "followedChanged"]);
    expect(session.unregisteredBeforeReturn()).toBe(true);
  });

  test("closes nothing for another session's removal, or an old one", async () => {
    const session = host({ [ISSUE]: 10 });

    expect(
      await closeReplayedRemoval({ data: removal("ses-2", [ISSUE]), seq: 20 }, session.removalHost)
    ).toEqual([]);
    expect(
      await closeReplayedRemoval({ data: removal("ses-1", [ISSUE]), seq: 10 }, session.removalHost)
    ).toEqual([]);
    expect(
      await closeReplayedRemoval(
        { data: JSON.stringify({ source: "dispatch" }), seq: 20 },
        session.removalHost
      )
    ).toEqual([]);
    expect([...session.open]).toEqual([ISSUE]);
    expect(session.calls).toEqual([]);
  });
});

describe("a one-time replay notice", () => {
  const route = (notice: ReplayNotice) => {
    const warned: string[] = [];
    const cards: unknown[] = [];
    routeReplayNotice(notice, {
      warn: (message) => warned.push(message),
      card: (text) => cards.push(decode(text)),
    });
    return { warned, cards };
  };

  test("that the listener does not replay is a warning", () => {
    expect(route({ kind: "not_served", message: "no replay" })).toEqual({
      warned: ["no replay"],
      cards: [],
    });
    expect(
      route({ kind: "unavailable", status: 503, error: "no grant", message: "unavailable" })
    ).toEqual({ warned: ["unavailable"], cards: [] });
  });

  test("that messages may be missing is a card for the session", () => {
    expect(route({ kind: "stream_recreated", message: "recreated" })).toEqual({
      warned: [],
      cards: [{ envoy: { notice: "stream recreated", detail: "recreated" } }],
    });
    expect(route({ kind: "retention_gap", message: "too old" })).toEqual({
      warned: [],
      cards: [{ envoy: { notice: "messages may be missing", detail: "too old" } }],
    });
  });
});
