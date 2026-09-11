import { describe, expect, test } from "bun:test";
import { dispatchDocumentSubject, dispatchIssueSubject } from "@legion/contracts";
import {
  dispatchSubscriptionTopic,
  dispatchTopicLabel,
  subscriptionRemovedTopics,
} from "../dispatch-subscribe";

describe("dispatchSubscriptionTopic", () => {
  test("returns the native issue topic from a successful dispatch result", () => {
    expect(
      dispatchSubscriptionTopic({
        issue: "DSP-42",
        topic: dispatchIssueSubject("DSP-42", ">"),
        ask: "ask-1",
      })
    ).toBe(dispatchIssueSubject("DSP-42", ">"));
  });

  test("refuses an absent, non-string, or foreign topic", () => {
    expect(dispatchSubscriptionTopic(undefined)).toBeNull();
    expect(dispatchSubscriptionTopic({ topic: 42 })).toBeNull();
    expect(
      dispatchSubscriptionTopic({ topic: "notifications.github.owner.repo.issue.42.>" })
    ).toBeNull();
  });
});

describe("dispatchTopicLabel", () => {
  test("renders an issue topic as its bare key", () => {
    expect(dispatchTopicLabel(dispatchIssueSubject("DSP-42", ">"))).toBe("DSP-42");
  });

  test("renders a document topic as project/slug", () => {
    expect(dispatchTopicLabel(dispatchDocumentSubject("CORE", "design-notes", ">"))).toBe(
      "CORE/design-notes"
    );
  });

  test("falls back to the raw topic for an unrecognised shape", () => {
    expect(dispatchTopicLabel("notifications.role.legion-controller")).toBe(
      "notifications.role.legion-controller"
    );
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
