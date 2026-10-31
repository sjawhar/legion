import { describe, expect, test } from "bun:test";
import { decode } from "@toon-format/toon";
import { type ConsumerGapNotice, gapNoticeText } from "../consumer-delivery";
import { closeReplayedRemoval } from "../delivery-ledger";

const ISSUE = "notifications.dispatch.issue.DSP-1.>";
const OTHER_ISSUE = "notifications.dispatch.issue.DSP-2.>";
/** When the stream stored the removal frame; the host's relay answers against it. */
const STORED_AT = Date.UTC(2026, 9, 26, 12, 0, 0);

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

  /** A host whose relay followed each topic before the frame was stored (true) or after it. */
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
        relay: { followedBefore: (topic: string) => followed[topic] === true },
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
      {
        data: removal("ses-1", [ISSUE, OTHER_ISSUE, "notifications.dispatch.issue.DSP-3.>"]),
        storedAtMs: STORED_AT,
      },
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
      await closeReplayedRemoval(
        { data: removal("ses-2", [ISSUE]), storedAtMs: STORED_AT },
        session.removalHost
      )
    ).toEqual([]);
    expect(
      await closeReplayedRemoval(
        { data: removal("ses-1", [OTHER_ISSUE]), storedAtMs: STORED_AT },
        later.removalHost
      )
    ).toEqual([]);
    expect(
      await closeReplayedRemoval(
        { data: JSON.stringify({ source: "dispatch" }), storedAtMs: STORED_AT },
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
