import { describe, expect, test } from "bun:test";

import type { AgentStreamFrame } from "@legion/contracts";

import {
  applyFrames,
  dispatchTurns,
  EMPTY_CONVERSATION,
  isRunning,
  toThreadMessages,
} from "./conversation";

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

describe("a settled message in the thread", () => {
  test("is not reopened by a later streaming snapshot, whatever its sequence number", () => {
    // The host delivers a message's last few updates after its end, so the late streaming
    // snapshot is the one with the HIGHER number. Letting it win leaves a finished turn
    // rendered as running, and assistant-ui then disables the composer.
    const state = applyFrames(EMPTY_CONVERSATION, [
      message(58, "a50", 50, "Hello there", false),
      message(59, "a50", 50, "Hello", true),
      message(60, "a50", 50, "Hello", true),
    ]);
    expect(state.messages).toHaveLength(1);
    expect(isRunning(state)).toBe(false);
    expect(toThreadMessages(state)[0]?.content).toEqual([{ text: "Hello there", type: "text" }]);
  });
});

describe("a frame this build cannot render", () => {
  test("is dropped rather than taken to assistant-ui", () => {
    // Every one of these throws inside assistant-ui's own conversion, which takes the whole
    // page to the route's error screen — on every load, because the session's replay serves
    // the same frame again.
    const bad = [
      { kind: "message", message: undefined, seq: 1, v: 1 },
      {
        kind: "message",
        message: { at: 10, id: "s10", parts: [], role: "system", streaming: false },
        seq: 2,
        v: 1,
      },
      {
        kind: "message",
        message: {
          at: 20,
          id: "u20",
          parts: [{ argsText: "{}", toolCallId: "c", toolName: "bash", type: "tool-call" }],
          role: "user",
          streaming: false,
        },
        seq: 3,
        v: 1,
      },
      {
        kind: "message",
        message: { at: 30, id: "a30", parts: [{ type: "video" }], role: "assistant" },
        seq: 4,
        v: 1,
      },
      {
        kind: "message",
        message: { at: 40, id: "a40", parts: [], role: "assistant" },
        seq: 5,
        v: 2,
      },
    ] as unknown as AgentStreamFrame[];
    const state = applyFrames(EMPTY_CONVERSATION, bad);
    expect(state.messages).toEqual([]);
    expect(() => toThreadMessages(state)).not.toThrow();

    // A good frame still applies after them.
    const good = applyFrames(state, [message(9, "a90", 90, "fine", false)]);
    expect(good.messages).toHaveLength(1);
  });
});

// A person's direct message from Dispatch becomes the session's own user turn, and the session
// tags that user message with the Dispatch message's id, so the view can show it once. The bus is
// open to any client, so the tag is honoured only where the publisher's contract puts it.
describe("a user message a person's Dispatch message became", () => {
  test("names the Dispatch message it delivered; a tag anywhere else costs the tag, not the message", () => {
    const frame = (seq: number, message: Record<string, unknown>): AgentStreamFrame =>
      ({ kind: "message", message, seq, v: 1 }) as unknown as AgentStreamFrame;
    const text = (value: string) => [{ text: value, type: "text" }];
    const state = applyFrames(EMPTY_CONVERSATION, [
      frame(1, {
        at: 10,
        dispatchMessageId: "m-1",
        id: "u10",
        parts: text("Where is the dashboard?"),
        role: "user",
        streaming: false,
      }),
      frame(2, { at: 20, id: "u20", parts: text("typed"), role: "user", streaming: false }),
      frame(3, {
        at: 30,
        dispatchMessageId: 7,
        id: "u30",
        parts: text("a number"),
        role: "user",
        streaming: false,
      }),
      frame(4, {
        at: 40,
        dispatchMessageId: "m-2",
        id: "a40",
        parts: text("an answer"),
        role: "assistant",
        streaming: false,
      }),
    ]);

    expect(dispatchTurns(state)).toEqual(
      new Map([["u10", { dispatchMessageId: "m-1", text: "Where is the dashboard?" }]])
    );
    expect(toThreadMessages(state).map((message) => message.id)).toEqual([
      "u10",
      "u20",
      "u30",
      "a40",
    ]);
  });
});
