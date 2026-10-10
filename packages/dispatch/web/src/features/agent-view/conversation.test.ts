import { describe, expect, test } from "bun:test";

import type { AgentStreamCommand, AgentStreamFrame } from "@legion/contracts";

import {
  applyFrames,
  currentModel,
  dispatchTurns,
  EMPTY_CONVERSATION,
  isRunning,
  sessionCommands,
  toThreadMessages,
} from "./conversation";

function message(
  seq: number,
  id: string,
  at: number,
  text: string,
  streaming: boolean,
  model?: string
): AgentStreamFrame {
  return {
    kind: "message",
    message: {
      at,
      id,
      parts: [{ text, type: "text" }],
      role: "assistant",
      streaming,
      ...(model === undefined ? {} : { model }),
    },
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

// The provider/model that produced an assistant turn (LEGION-548): validated like any other
// field a publisher any client can write might get wrong, carried to the renderer through
// assistant-ui's metadata channel, and read back for the live view's header.
describe("the model that produced an assistant turn", () => {
  test("isRenderableFrame refuses a non-string model", () => {
    const bad = {
      kind: "message",
      message: { at: 10, id: "a10", model: 7, parts: [], role: "assistant", streaming: false },
      seq: 1,
      v: 1,
    } as unknown as AgentStreamFrame;
    expect(applyFrames(EMPTY_CONVERSATION, [bad]).messages).toEqual([]);
  });

  test("toThreadMessages carries it in metadata.custom for the renderer to read back", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [
      message(1, "a50", 50, "Hello", false, "anthropic/claude-opus-5"),
    ]);
    expect(toThreadMessages(state)[0]?.metadata?.custom).toEqual({
      model: "anthropic/claude-opus-5",
    });
  });

  test("a turn with none carries no metadata at all", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [message(1, "a50", 50, "Hello", false)]);
    expect(toThreadMessages(state)[0]?.metadata).toBeUndefined();
  });

  // The bus is open to any client, so a user-turn frame naming a model is a claim this build
  // does not honour: pi-envoy's own host object never puts one on a user message, and the
  // contract documents the field as absent there, so a forged one is rendered with no metadata
  // rather than shown as if the viewer had typed it with a model attached.
  test("a forged user-turn frame naming a model carries no metadata either", () => {
    const forged = {
      kind: "message",
      message: {
        at: 10,
        id: "u10",
        model: "anthropic/claude-opus-5",
        parts: [{ text: "ship it", type: "text" }],
        role: "user",
        streaming: false,
      },
      seq: 1,
      v: 1,
    } as unknown as AgentStreamFrame;
    const state = applyFrames(EMPTY_CONVERSATION, [forged]);
    expect(toThreadMessages(state)[0]?.metadata).toBeUndefined();
  });

  test("currentModel is the most recent assistant turn's own report, whatever it is", () => {
    const sessionSwitchedModel = applyFrames(EMPTY_CONVERSATION, [
      message(1, "a10", 10, "first", false, "anthropic/claude-opus-5"),
      message(2, "a20", 20, "second", false, "anthropic/claude-sonnet-5"),
    ]);
    expect(currentModel(sessionSwitchedModel)).toBe("anthropic/claude-sonnet-5");

    // The newest turn reported none: shown as nothing, not as the older turn's model, which
    // could already be stale by the time anyone reads it.
    const latestSilent = applyFrames(EMPTY_CONVERSATION, [
      message(1, "a10", 10, "first", false, "anthropic/claude-opus-5"),
      message(2, "a20", 20, "second", false),
    ]);
    expect(currentModel(latestSilent)).toBeUndefined();

    // A user turn after the last assistant reply never resets what the header shows.
    const userAfter = applyFrames(EMPTY_CONVERSATION, [
      message(1, "a10", 10, "first", false, "anthropic/claude-opus-5"),
      {
        kind: "message",
        message: {
          at: 20,
          id: "u20",
          parts: [{ text: "go on", type: "text" }],
          role: "user",
          streaming: false,
        },
        seq: 2,
        v: 1,
      },
    ]);
    expect(currentModel(userAfter)).toBe("anthropic/claude-opus-5");
  });

  test("a session whose client never reports one has nothing to show", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [message(1, "a10", 10, "first", false)]);
    expect(currentModel(state)).toBeUndefined();
  });
});

function commandsFrame(seq: number, commands: readonly AgentStreamCommand[]): AgentStreamFrame {
  return { commands, kind: "commands", seq, v: 1 };
}

const LISTED: readonly AgentStreamCommand[] = [
  { description: "Compact the session's context", name: "compact", source: "builtin" },
  { description: "Start a new session", name: "new", source: "builtin", terminalOnly: true },
  { name: "skill:dispatch", source: "skill" },
];

// The slash commands a session takes from Dispatch (LEGION-394) arrive as one `commands` frame
// holding the whole list. They are what the composer completes, never a turn of the transcript.
describe("the session's slash commands", () => {
  test("are the latest list the session sent, kept through later turns and out of the transcript", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [
      commandsFrame(1, LISTED),
      message(2, "a50", 50, "Hello", false),
    ]);
    expect(sessionCommands(state)).toEqual(LISTED);
    expect(toThreadMessages(state).map((entry) => entry.id)).toEqual(["a50"]);
  });

  test("are absent for a session that sent no list, which is one that cannot take any", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [message(1, "a50", 50, "Hello", false)]);
    expect(sessionCommands(state)).toBeUndefined();
  });

  test("a newer list replaces the one before it, and a list that lost its race is ignored", () => {
    const newer = [{ name: "jobs", source: "builtin" }] as const satisfies AgentStreamCommand[];
    const state = applyFrames(EMPTY_CONVERSATION, [
      commandsFrame(5, LISTED),
      commandsFrame(7, newer),
      commandsFrame(6, LISTED),
    ]);
    expect(sessionCommands(state)).toEqual(newer);
  });

  test("a list with any entry this build cannot read is dropped whole", () => {
    // Each fails one rule the contract states; the list the session sent before stays.
    const bad = [
      { commands: "compact", kind: "commands", seq: 2, v: 1 },
      { kind: "commands", seq: 3, v: 1 },
      { commands: [...LISTED, null], kind: "commands", seq: 4, v: 1 },
      { commands: [{ name: 7, source: "builtin" }], kind: "commands", seq: 5, v: 1 },
      { commands: [{ name: "compact", source: "terminal" }], kind: "commands", seq: 6, v: 1 },
      {
        commands: [{ name: "new", source: "builtin", terminalOnly: false }],
        kind: "commands",
        seq: 7,
        v: 1,
      },
      {
        commands: [{ description: 7, name: "compact", source: "builtin" }],
        kind: "commands",
        seq: 8,
        v: 1,
      },
      { commands: [], kind: "commands", seq: "9", v: 1 },
      { commands: [], kind: "commands", seq: 10, v: 2 },
      // A person types a command as one token, so the session never lists an empty name or one
      // holding whitespace (pi-envoy's `setCommands` drops both); one on the bus is forged.
      { commands: [{ name: "", source: "builtin" }], kind: "commands", seq: 11, v: 1 },
      { commands: [{ name: " ", source: "builtin" }], kind: "commands", seq: 12, v: 1 },
      { commands: [{ name: "two words", source: "prompt" }], kind: "commands", seq: 13, v: 1 },
    ] as unknown as AgentStreamFrame[];
    const state = applyFrames(EMPTY_CONVERSATION, [commandsFrame(1, LISTED), ...bad]);
    expect(sessionCommands(state)).toEqual(LISTED);
  });
});
