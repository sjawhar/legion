import { describe, expect, test } from "bun:test";
import { dispatchFollowNotice, subscriptionRemovedTopics } from "../dispatch-subscribe";

describe("dispatchFollowNotice", () => {
  test("names the ask, its issue, and the opt-in subscribe line for a result that follows an ask", () => {
    expect(
      dispatchFollowNotice({ issue: "DSP-42", ask: "ask-1", follows: { ask: "ask-1" } })
    ).toEqual({
      ask: "ask-1",
      text: "Following ask ask-1 on DSP-42: its answer and replies reach you directly (dispatch_follow unfollow to stop). For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>.",
    });
  });

  test("names a project document by project/slug with the document topic", () => {
    expect(
      dispatchFollowNotice({
        project: "CORE",
        document: "CORE/design-notes",
        artifact: "artifact-1",
        ask: "ask-1",
        follows: { ask: "ask-1" },
      })?.text
    ).toBe(
      "Following ask ask-1 on CORE/design-notes: its answer and replies reach you directly (dispatch_follow unfollow to stop). For every event on CORE/design-notes: envoy_subscribe notifications.dispatch.document.CORE.design-notes.>."
    );
  });

  test("returns null for reads, unfollows, and results without an owner", () => {
    expect(dispatchFollowNotice(undefined)).toBeNull();
    expect(dispatchFollowNotice({ issue: "DSP-42" })).toBeNull();
    expect(dispatchFollowNotice({ ask: "ask-1" })).toBeNull();
    expect(dispatchFollowNotice({ follows: { ask: "ask-1" } })).toBeNull();
    expect(dispatchFollowNotice({ follows: { ask: 7 }, issue: "DSP-42" })).toBeNull();
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
