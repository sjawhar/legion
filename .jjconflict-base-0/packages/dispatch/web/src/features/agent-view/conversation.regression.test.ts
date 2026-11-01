import { describe, expect, test } from "bun:test";

import type { AgentStreamFrame } from "@legion/contracts";

import { applyFrames, EMPTY_CONVERSATION, toThreadMessages } from "./conversation";

/**
 * assistant-ui accepts `status` on an assistant message only, and throws `status is only
 * supported for assistant messages` otherwise — which took down the whole page, not one turn,
 * the first time a human's own message reached the thread.
 */
describe("a human's own message in the thread", () => {
  test("carries no status", () => {
    const user: AgentStreamFrame = {
      kind: "message",
      message: {
        at: 10,
        id: "u10",
        parts: [{ text: "hello", type: "text" }],
        role: "user",
        streaming: false,
      },
      seq: 1,
      v: 1,
    };
    const [message] = toThreadMessages(applyFrames(EMPTY_CONVERSATION, [user]));
    expect(message?.role).toBe("user");
    expect(message === undefined ? true : "status" in message).toBe(false);
  });
});
