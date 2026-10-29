import { describe, expect, test } from "bun:test";

import {
  AGENT_STREAM_LIMITS,
  AGENT_STREAM_TRUNCATION_SUFFIX,
  type AgentStreamCommand,
  type AgentStreamFrame,
  type AgentStreamSettings,
  type AgentStreamSettingsReported,
} from "@legion/contracts";

import {
  applyFrame,
  applyFrames,
  currentModel,
  dispatchTurns,
  EMPTY_CONVERSATION,
  sessionCommands,
  sessionSettingsView,
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
    expect(state.messages[0]?.streaming).toBe(false);
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

  test("a message still streaming renders as running", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [message(1, "a50", 50, "Hel", true)]);
    expect(state.messages[0]?.streaming).toBe(true);
    expect(toThreadMessages(state)[0]?.status).toEqual({ type: "running" });
  });
});

describe("a settled message in the thread", () => {
  test("is not reopened by a later streaming snapshot, whatever its sequence number", () => {
    // The host delivers a message's last few updates after its end, so the late streaming
    // snapshot is the one with the HIGHER number. Letting it win would leave a finished turn
    // rendered as still streaming.
    const state = applyFrames(EMPTY_CONVERSATION, [
      message(58, "a50", 50, "Hello there", false),
      message(59, "a50", 50, "Hello", true),
      message(60, "a50", 50, "Hello", true),
    ]);
    expect(state.messages).toHaveLength(1);
    expect(toThreadMessages(state)[0]?.status).toEqual({ reason: "stop", type: "complete" });
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
  test("are the latest list the session sent, kept through later messages and tool results", () => {
    const result: AgentStreamFrame = {
      kind: "tool-result",
      result: { at: 60, isError: false, output: "ok", toolCallId: "c1", toolName: "bash" },
      seq: 3,
      v: 1,
    };
    const state = applyFrames(EMPTY_CONVERSATION, [
      commandsFrame(1, LISTED),
      message(2, "a50", 50, "Hello", false),
      result,
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
      // holding whitespace or a control character (pi-envoy's `setCommands` drops them); one on
      // the bus is forged.
      { commands: [{ name: "", source: "builtin" }], kind: "commands", seq: 11, v: 1 },
      { commands: [{ name: " ", source: "builtin" }], kind: "commands", seq: 12, v: 1 },
      { commands: [{ name: "two words", source: "prompt" }], kind: "commands", seq: 13, v: 1 },
      { commands: [{ name: "nul\u0000name", source: "prompt" }], kind: "commands", seq: 14, v: 1 },
    ] as unknown as AgentStreamFrame[];
    const state = applyFrames(EMPTY_CONVERSATION, [commandsFrame(1, LISTED), ...bad]);
    expect(sessionCommands(state)).toEqual(LISTED);
  });

  test("drops a command list over its count, name or publisher-capped description bound", () => {
    const cappedDescription =
      "d".repeat(AGENT_STREAM_LIMITS.commandDescriptionChars) + AGENT_STREAM_TRUNCATION_SUFFIX;
    const maximum = Array.from({ length: AGENT_STREAM_LIMITS.commands }, (_, index) => ({
      description: cappedDescription,
      name: `command-${index}`,
      source: "extension" as const,
    }));
    const bad = [
      commandsFrame(2, [...maximum, { name: "one-too-many", source: "extension" }]),
      commandsFrame(3, [
        { name: "n".repeat(AGENT_STREAM_LIMITS.commandNameChars + 1), source: "extension" },
      ]),
      commandsFrame(4, [
        {
          description: "d".repeat(
            AGENT_STREAM_LIMITS.commandDescriptionChars + AGENT_STREAM_TRUNCATION_SUFFIX.length + 1
          ),
          name: "too-long-description",
          source: "extension",
        },
      ]),
    ];

    expect(
      sessionCommands(applyFrames(EMPTY_CONVERSATION, [commandsFrame(1, LISTED), ...bad]))
    ).toEqual(LISTED);
    expect(sessionCommands(applyFrames(EMPTY_CONVERSATION, [commandsFrame(1, maximum)]))).toEqual(
      maximum
    );
  });
});

/** A reported `settings` frame: model-a, thinking `auto`, unless `fields` says otherwise, which
 *  may break the contract on purpose. */
function settingsFrame(
  seq: number,
  fields: Record<string, unknown> = {}
): AgentStreamSettingsReported {
  return {
    kind: "settings",
    model: "anthropic/model-a",
    models: ["anthropic/model-a", "anthropic/model-b", "openai/model-c"],
    reported: true,
    seq,
    thinking: { level: "auto", levels: ["auto", "low", "high"] },
    v: 1,
    ...fields,
  } as AgentStreamSettingsReported;
}

/** The built-ins a pick runs, as a session that runs them from Dispatch lists them. */
const SWITCHES: readonly AgentStreamCommand[] = [
  { name: "model", source: "builtin" },
  { name: "effort", source: "builtin" },
];

// The model and thinking level the session runs now (INFRA-195) arrive as one `settings` frame
// holding the whole state. The header shows the newest, never a turn of the transcript.
describe("the session's settings", () => {
  test("are the newest frame by seq, kept through later messages and command lists", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [
      settingsFrame(5),
      settingsFrame(7, { model: "anthropic/model-b" }),
      settingsFrame(6, { model: "openai/model-c" }),
      message(8, "a50", 50, "Hello", false, "anthropic/model-a"),
      commandsFrame(9, SWITCHES),
    ]);
    expect(state.settings).toEqual(settingsFrame(7, { model: "anthropic/model-b" }));
    expect(toThreadMessages(state).map((entry) => entry.id)).toEqual(["a50"]);
  });

  test("a frame with any field of the wrong type is dropped, and the one before it stays", () => {
    const bad = [
      settingsFrame(2, { reported: "yes" }),
      settingsFrame(3, { reported: undefined }),
      settingsFrame(4, { model: 7 }),
      settingsFrame(5, { model: null }),
      settingsFrame(6, { models: "anthropic/model-a" }),
      settingsFrame(7, { models: undefined }),
      settingsFrame(8, { models: ["anthropic/model-a", 7] }),
      settingsFrame(9, { thinking: undefined }),
      settingsFrame(10, { thinking: "auto" }),
      settingsFrame(11, { thinking: { levels: ["auto"] } }),
      settingsFrame(12, { thinking: { level: 3, levels: ["auto"] } }),
      settingsFrame(13, { thinking: { level: "auto" } }),
      settingsFrame(14, { thinking: { level: "auto", levels: ["auto", null] } }),
      settingsFrame(15, { thinking: ["auto"] }),
      settingsFrame(16, { seq: "16" }),
      settingsFrame(17, { v: 2 }),
    ];
    const before = settingsFrame(1);
    let state = applyFrame(EMPTY_CONVERSATION, before);
    for (const frame of bad) {
      state = applyFrame(state, frame);
      expect(state.settings).toEqual(before);
    }
  });

  test("every shape the contract allows is taken", () => {
    const allowed: AgentStreamSettings[] = [
      settingsFrame(1, { model: undefined }),
      settingsFrame(2, { thinking: null }),
      settingsFrame(3, { thinking: { level: null, levels: ["low", "high"] } }),
      settingsFrame(4, { models: [] }),
      { kind: "settings", reported: false, seq: 5, v: 1 },
    ];
    let state = EMPTY_CONVERSATION;
    for (const frame of allowed) {
      state = applyFrame(state, frame);
      expect(state.settings).toEqual(frame);
    }
  });

  test("a replay rebuilds them from its own frames alone", () => {
    // A session restarted counts its seq from the start again, and a plugin older than the frame
    // sends none: the rebuild holds exactly what the replay says, whatever was shown before.
    const before = applyFrames(EMPTY_CONVERSATION, [
      settingsFrame(40, { model: "openai/model-c" }),
    ]);
    expect(before.settings).toEqual(settingsFrame(40, { model: "openai/model-c" }));

    const restarted = applyFrames(EMPTY_CONVERSATION, [
      settingsFrame(2),
      commandsFrame(3, SWITCHES),
    ]);
    expect(restarted.settings).toEqual(settingsFrame(2));

    const older = applyFrames(EMPTY_CONVERSATION, [message(1, "a50", 50, "Hello", false)]);
    expect(older.settings).toBeUndefined();
  });
});

// Each row of what the header shows of the session's model and thinking level, and the notice
// that says why a session shows less (INFRA-195, decision 7).
describe("what the header shows of the session's settings", () => {
  const lastTurn = message(1, "a10", 10, "Done.", false, "anthropic/old-model");

  test("a session not answering the live view shows the last turn's model and no notice", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [lastTurn, settingsFrame(2)]);
    expect(sessionSettingsView(state, false, 1)).toEqual({
      model: "anthropic/old-model",
      notice: undefined,
      reported: false,
    });
  });

  test("a session whose replay has not arrived shows the last turn's model and no notice", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [lastTurn]);
    expect(sessionSettingsView(state, true, 0)).toEqual({
      model: "anthropic/old-model",
      notice: undefined,
      reported: false,
    });
    expect(sessionSettingsView(state, undefined, 0)).toEqual({
      model: "anthropic/old-model",
      notice: undefined,
      reported: false,
    });
  });

  test("a replay with no settings frame asks for a restart on a newer pi-envoy", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [lastTurn, commandsFrame(2, SWITCHES)]);
    expect(sessionSettingsView(state, true, 1)).toEqual({
      model: "anthropic/old-model",
      notice:
        "This session does not report its model and thinking level. Restart it on a newer pi-envoy to show and switch them here.",
      reported: false,
    });
  });

  test("an unreported frame shows the last turn's model and says Oh My Pi does not tell", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [
      lastTurn,
      { kind: "settings", reported: false, seq: 2, v: 1 },
      commandsFrame(3, SWITCHES),
    ]);
    expect(sessionSettingsView(state, true, 1)).toEqual({
      model: "anthropic/old-model",
      notice: "This session's Oh My Pi does not tell Dispatch its model and thinking level.",
      reported: false,
    });
  });

  test("reported settings without the built-ins listed offer no picker and say so", () => {
    const terminalOnly = applyFrames(EMPTY_CONVERSATION, [
      lastTurn,
      settingsFrame(2),
      commandsFrame(3, [
        { name: "model", source: "builtin", terminalOnly: true },
        { name: "effort", source: "builtin" },
      ]),
    ]);
    expect(sessionSettingsView(terminalOnly, true, 1)).toEqual({
      modelPicker: false,
      notice: "This session switches its model and thinking level only at its own terminal.",
      reported: true,
      settings: settingsFrame(2),
      thinkingPicker: true,
    });

    // No command list at all is a host that cannot run typed input; a `model` that is not the
    // built-in is not what a pick runs.
    for (const commands of [[], [{ name: "model", source: "extension" }]] as const) {
      const state = applyFrames(EMPTY_CONVERSATION, [
        settingsFrame(2),
        ...(commands.length === 0 ? [] : [commandsFrame(3, commands)]),
      ]);
      expect(sessionSettingsView(state, true, 1)).toEqual({
        modelPicker: false,
        notice: "This session switches its model and thinking level only at its own terminal.",
        reported: true,
        settings: settingsFrame(2),
        thinkingPicker: false,
      });
    }
  });

  test("reported settings with both built-ins listed offer both pickers and no notice", () => {
    const state = applyFrames(EMPTY_CONVERSATION, [
      lastTurn,
      settingsFrame(2, { model: "anthropic/model-b" }),
      commandsFrame(3, SWITCHES),
    ]);
    // The reported model wins over the last turn's: the turn ran before the switch.
    expect(sessionSettingsView(state, true, 1)).toEqual({
      modelPicker: true,
      notice: undefined,
      reported: true,
      settings: settingsFrame(2, { model: "anthropic/model-b" }),
      thinkingPicker: true,
    });
    expect(currentModel(state)).toBe("anthropic/old-model");
  });
});
