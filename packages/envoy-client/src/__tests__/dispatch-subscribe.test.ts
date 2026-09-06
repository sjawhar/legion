import { describe, expect, it } from "bun:test";
import {
  dispatchSubscriptionTopic,
  dispatchThreadTopic,
  isDispatchTool,
} from "../dispatch-subscribe";

describe("isDispatchTool", () => {
  it("matches only the native tool name", () => {
    expect(isDispatchTool("dispatch")).toBe(true);
  });

  it("does not match prefixed, suffixed, or unrelated tools", () => {
    expect(isDispatchTool("envoy_dispatch")).toBe(false);
    expect(isDispatchTool("mcp__envoy__dispatch")).toBe(false);
    expect(isDispatchTool("dispatcher")).toBe(false);
    expect(isDispatchTool("dispatch_thread")).toBe(false);
    expect(isDispatchTool("bash")).toBe(false);
  });
});

describe("dispatchThreadTopic", () => {
  it("builds the wildcard thread topic", () => {
    expect(dispatchThreadTopic("sjawhar", "legion", 123)).toBe(
      "notifications.github.sjawhar.legion.issue.123.>"
    );
  });
});

describe("dispatchSubscriptionTopic", () => {
  it("derives the topic from a dispatch tool result JSON", () => {
    const output = JSON.stringify({
      thread: 742,
      url: "https://github.com/sjawhar/legion/issues/742",
    });
    expect(dispatchSubscriptionTopic("dispatch", output)).toBe(
      "notifications.github.sjawhar.legion.issue.742.>"
    );
  });

  it("derives owner/repo/number purely from the issue URL", () => {
    // Even if a stale/incorrect JSON `thread` were present, the URL is canonical.
    const output = '{"thread":1,"url":"https://github.com/acme/Widgets/issues/55"}';
    expect(dispatchSubscriptionTopic("dispatch", output)).toBe(
      "notifications.github.acme.Widgets.issue.55.>"
    );
  });

  it("returns null for non-dispatch tools even with a github URL present", () => {
    const output = '{"url":"https://github.com/sjawhar/legion/issues/9"}';
    expect(dispatchSubscriptionTopic("envoy_subscribe", output)).toBeNull();
  });

  it("returns null when the output has no github issue URL", () => {
    expect(dispatchSubscriptionTopic("dispatch", "created thread 5")).toBeNull();
    expect(dispatchSubscriptionTopic("dispatch", "")).toBeNull();
    expect(dispatchSubscriptionTopic("dispatch", "not json at all")).toBeNull();
  });

  it("ignores pull-request URLs (only issue threads carry dispatch replies)", () => {
    const output = '{"url":"https://github.com/sjawhar/legion/pull/100"}';
    expect(dispatchSubscriptionTopic("dispatch", output)).toBeNull();
  });
});
