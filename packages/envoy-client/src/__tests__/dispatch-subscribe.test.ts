import { describe, expect, test } from "bun:test";
import { documentTopicOf, issueTopic } from "../dispatch-owner";
import {
  followedAsk,
  followNoticeText,
  subscriptionCovers,
  subscriptionRemovedTopics,
} from "../dispatch-subscribe";

/** The follow notice for `details`, which must follow an ask, given the session's `topics`. */
function notice(details: unknown, topics: readonly string[] | null): string {
  const followed = followedAsk(details);
  if (followed === null) throw new Error("details follow no ask");
  return followNoticeText(followed, topics);
}

describe("subscriptionCovers", () => {
  test("a topic covers an owner when it receives every event on it, as the listener matches", () => {
    const issue = issueTopic("DSP-42");
    for (const topic of [
      "notifications.dispatch.issue.DSP-42.>",
      "notifications.dispatch.issue.*.>",
      "notifications.dispatch.issue.>",
      "notifications.dispatch.>",
      "notifications.>",
      ">",
    ]) {
      expect([topic, subscriptionCovers(topic, issue)]).toEqual([topic, true]);
    }
    for (const topic of [
      "notifications.dispatch.issue.DSP-42",
      // An event type can span tokens (`ask.answered`), which `*` stops short of.
      "notifications.dispatch.issue.DSP-42.*",
      "notifications.dispatch.issue.DSP-42.ask.>",
      "notifications.dispatch.issue.DSP-4.>",
      "notifications.dispatch.issue.DSP-420.>",
      "notifications.dispatch.issue.DSP-42.comment.created",
      "notifications.dispatch.document.DSP.spec.>",
      "notifications.agent.ses-1",
      "notifications.role.reviewer",
    ]) {
      expect([topic, subscriptionCovers(topic, issue)]).toEqual([topic, false]);
    }
    const document = documentTopicOf("CORE", "runbook-md");
    expect(subscriptionCovers("notifications.dispatch.document.CORE.runbook-md.>", document)).toBe(
      true
    );
    expect(subscriptionCovers("notifications.dispatch.document.CORE.*.>", document)).toBe(true);
    expect(subscriptionCovers("notifications.dispatch.issue.>", document)).toBe(false);
  });
});

describe("the follow notice", () => {
  test("names the ask, its issue, and the subscribe line only when no subscription covers the issue", () => {
    const details = { issue: "DSP-42", ask: "ask-1", follows: { ask: "ask-1" } };
    const follows =
      "Following ask ask-1 on DSP-42: its answer and replies reach you directly (dispatch follow --ask ask-1 --action unfollow to stop).";
    const subscribe =
      " For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>.";
    expect(followedAsk(details)).toEqual({ ask: "ask-1", owner: issueTopic("DSP-42") });
    expect(notice(details, ["notifications.agent.ses-1"])).toBe(`${follows}${subscribe}`);
    expect(notice(details, [])).toBe(`${follows}${subscribe}`);
    expect(notice(details, ["notifications.dispatch.issue.DSP-42.>"])).toBe(follows);
    expect(notice(details, ["notifications.dispatch.>"])).toBe(follows);
    // Subscriptions that could not be read say nothing about what the session receives.
    expect(notice(details, null)).toBe(follows);
  });

  test("names a project document by project/slug with the document topic", () => {
    const details = {
      project: "CORE",
      document: "CORE/design-notes",
      artifact: "artifact-1",
      ask: "ask-1",
      follows: { ask: "ask-1" },
    };
    expect(notice(details, [])).toBe(
      "Following ask ask-1 on CORE/design-notes: its answer and replies reach you directly (dispatch follow --ask ask-1 --action unfollow to stop). For every event on CORE/design-notes: envoy_subscribe notifications.dispatch.document.CORE.design-notes.>."
    );
    expect(notice(details, ["notifications.dispatch.document.CORE.design-notes.>"])).not.toContain(
      "envoy_subscribe"
    );
  });

  test("returns null for reads, unfollows, and results without an owner", () => {
    expect(followedAsk(undefined)).toBeNull();
    expect(followedAsk({ issue: "DSP-42" })).toBeNull();
    expect(followedAsk({ ask: "ask-1" })).toBeNull();
    expect(followedAsk({ follows: { ask: "ask-1" } })).toBeNull();
    expect(followedAsk({ follows: { ask: 7 }, issue: "DSP-42" })).toBeNull();
  });
});

describe("subscriptionRemovedTopics", () => {
  function envelope(payload: unknown): string {
    return JSON.stringify({ source: "dispatch", payload: JSON.stringify(payload) });
  }

  test("extracts the removed topics from a subscription.removed envelope naming this session", () => {
    const raw = envelope({
      issue_key: "DSP-1",
      type: "subscription.removed",
      actor: { kind: "user", id: "alice" },
      payload: {
        session_id: "ses_target",
        by: { kind: "user", id: "alice" },
        topics: ["notifications.dispatch.issue.DSP-1.>"],
      },
    });

    expect(subscriptionRemovedTopics(raw, "ses_target")).toEqual([
      "notifications.dispatch.issue.DSP-1.>",
    ]);
  });

  test("returns undefined for a removal naming a different session — this event also reaches the issue's own topic, so every other subscriber must ignore it", () => {
    const raw = envelope({
      issue_key: "DSP-1",
      type: "subscription.removed",
      actor: { kind: "user", id: "alice" },
      payload: {
        session_id: "ses_target",
        by: { kind: "user", id: "alice" },
        topics: ["notifications.dispatch.issue.DSP-1.>"],
      },
    });

    expect(subscriptionRemovedTopics(raw, "ses_other")).toBeUndefined();
  });

  test("returns undefined for a different Dispatch event type", () => {
    const raw = envelope({
      issue_key: "DSP-1",
      type: "comment.created",
      actor: { kind: "user", id: "alice" },
      payload: {},
    });

    expect(subscriptionRemovedTopics(raw, "ses_target")).toBeUndefined();
  });

  test("returns undefined for a non-dispatch envelope, and for malformed JSON at either layer", () => {
    expect(
      subscriptionRemovedTopics(JSON.stringify({ source: "agent", payload: "{}" }), "ses_target")
    ).toBeUndefined();
    expect(subscriptionRemovedTopics("not json", "ses_target")).toBeUndefined();
    expect(
      subscriptionRemovedTopics(
        JSON.stringify({ source: "dispatch", payload: "not json" }),
        "ses_target"
      )
    ).toBeUndefined();
  });
});
