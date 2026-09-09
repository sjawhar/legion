import { describe, expect, test } from "bun:test";
import { dispatchSubscriptionTopic } from "../dispatch-subscribe";

describe("dispatchSubscriptionTopic", () => {
  test("returns the native issue topic from a successful dispatch result", () => {
    expect(
      dispatchSubscriptionTopic({
        issue: "DSP-42",
        topic: "notifications.dispatch.issue.DSP-42.>",
        ask: "ask-1",
      })
    ).toBe("notifications.dispatch.issue.DSP-42.>");
  });

  test("refuses an absent, non-string, or foreign topic", () => {
    expect(dispatchSubscriptionTopic(undefined)).toBeNull();
    expect(dispatchSubscriptionTopic({ topic: 42 })).toBeNull();
    expect(
      dispatchSubscriptionTopic({ topic: "notifications.github.owner.repo.issue.42.>" })
    ).toBeNull();
  });
});
