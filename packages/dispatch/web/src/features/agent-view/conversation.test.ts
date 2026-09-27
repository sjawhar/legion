import { describe, expect, test } from "bun:test";

import type { AgentStreamFrame } from "@legion/contracts";

import { applyFrames, EMPTY_CONVERSATION, isRunning, toThreadMessages } from "./conversation";

function message(
  seq: number,
  id: string,
  at: number,
  text: string,
  streaming: boolean
): AgentStreamFrame {
  return {
    kind: "message",
    message: { at, id, parts: [{ text, type: "text" }], role: "assistant", streaming },
    seq,
    v: 1,
  };
}

describe("the live conversation", () => {
  test("a snapshot that lost its race never overwrites a newer one", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [
      message(2, "a50", 50, "Hello there", false),
      message(1, "a50", 50, "Hel", true),
    ]);
    expect(state.messages).toHaveLength(1);
    expect(toThreadMessages(state)[0]?.content).toEqual([{ text: "Hello there", type: "text" }]);
    expect(isRunning(state)).toBe(false);
  });

  test("messages are ordered by the session's own clock, not by arrival", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [
      message(1, "a90", 90, "second", false),
      message(2, "u10", 10, "first", false),
    ]);
    expect(toThreadMessages(state).map((entry) => entry.id)).toEqual(["u10", "a90"]);
  });

  test("a tool result finds its call whichever order the two arrive in", () => {
    const call: AgentStreamFrame = {
      kind: "message",
      message: {
        at: 50,
        id: "a50",
        parts: [{ argsText: "{}", toolCallId: "c1", toolName: "bash", type: "tool-call" }],
        role: "assistant",
        streaming: false,
      },
      seq: 2,
      v: 1,
    };
    const result: AgentStreamFrame = {
      kind: "tool-result",
      result: { at: 60, isError: true, output: "nope", toolCallId: "c1", toolName: "bash" },
      seq: 1,
      v: 1,
    };
    for (const frames of [
      [call, result],
      [result, call],
    ]) {
      const state = applyFrames(EMPTY_CONVERSATION, frames);
      expect(toThreadMessages(state)[0]?.content).toEqual([
        {
          argsText: "{}",
          isError: true,
          result: "nope",
          toolCallId: "c1",
          toolName: "bash",
          type: "tool-call",
        },
      ]);
    }
  });

  test("a message still streaming keeps the thread running", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [message(1, "a50", 50, "Hel", true)]);
    expect(isRunning(state)).toBe(true);
    expect(toThreadMessages(state)[0]?.status).toEqual({ type: "running" });
  });
});
