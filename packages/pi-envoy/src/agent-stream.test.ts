import { describe, expect, test } from "bun:test";

import {
  AGENT_STREAM_LIMITS,
  AGENT_STREAM_TRUNCATION_SUFFIX,
  type AgentStreamFrame,
} from "@legion/contracts";

import { AgentStreamPublisher } from "./agent-stream";

const SUBJECT = "agentstream.s1.frames";

function harness(startAt = 1_000) {
  const published: AgentStreamFrame[] = [];
  let clock = startAt;
  const publisher = new AgentStreamPublisher({
    now: () => clock,
    publish: (_subject, payload) => {
      published.push(JSON.parse(payload) as AgentStreamFrame);
    },
  });
  return {
    advance: (ms: number) => {
      clock += ms;
    },
    published,
    publisher,
  };
}

/** An assistant message as the host hands it over: `timestamp` is stable for its whole stream. */
function assistant(timestamp: number, content: unknown[]) {
  return { content, role: "assistant", timestamp };
}

describe("AgentStreamPublisher", () => {
  test("a streamed assistant reply is one message the viewer can key by id", () => {
    const { advance, published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(SUBJECT, assistant(50, []), true);
    advance(AGENT_STREAM_LIMITS.snapshotIntervalMs);
    publisher.record(SUBJECT, assistant(50, [{ text: "Hel", type: "text" }]), true);
    advance(AGENT_STREAM_LIMITS.snapshotIntervalMs);
    publisher.record(SUBJECT, assistant(50, [{ text: "Hello", type: "text" }]), false);

    const ids = new Set(
      published.map((frame) => (frame.kind === "message" ? frame.message.id : "tool"))
    );
    expect(ids.size).toBe(1);
    expect(published.map((frame) => frame.seq)).toEqual([1, 2, 3]);
    const last = published.at(-1);
    expect(last?.kind === "message" && last.message.streaming).toBe(false);
    expect(last?.kind === "message" && last.message.parts).toEqual([
      { text: "Hello", type: "text" },
    ]);
  });

  test("an unwatched session neither publishes nor answers a replay, and answers once armed", () => {
    const { published, publisher } = harness();
    publisher.record(SUBJECT, { content: "hi", role: "user", timestamp: 10 }, false);
    publisher.record(SUBJECT, assistant(20, [{ text: "there", type: "text" }]), false);
    expect(published).toEqual([]);
    // Handing the ring to a caller is publishing too: one bare replay request must not draw
    // the conversation out of a session nobody has opened.
    expect(publisher.replay("s1").frames).toEqual([]);

    // The ring kept filling while nobody watched, which is what lets the first viewer to open
    // this session see the turn it is already in.
    publisher.noteViewer();
    expect(
      publisher
        .replay("s1")
        .frames.map((frame) => (frame.kind === "message" ? frame.message.role : "tool"))
    ).toEqual(["user", "assistant"]);
  });

  test("the watch window expires, and a fresh viewer re-arms it", () => {
    const { advance, published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(SUBJECT, assistant(10, [{ text: "one", type: "text" }]), false);
    advance(60_000);
    publisher.record(SUBJECT, assistant(20, [{ text: "two", type: "text" }]), false);
    expect(published).toHaveLength(1);

    publisher.noteViewer();
    publisher.record(SUBJECT, assistant(30, [{ text: "three", type: "text" }]), false);
    expect(published).toHaveLength(2);
  });

  test("token deltas coalesce, and the settled message is always published", () => {
    const { advance, published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(SUBJECT, assistant(50, [{ text: "a", type: "text" }]), true);
    advance(1);
    publisher.record(SUBJECT, assistant(50, [{ text: "ab", type: "text" }]), true);
    advance(1);
    publisher.record(SUBJECT, assistant(50, [{ text: "abc", type: "text" }]), true);
    expect(published).toHaveLength(1);

    publisher.record(SUBJECT, assistant(50, [{ text: "abcd", type: "text" }]), false);
    expect(published).toHaveLength(2);
    const last = published.at(-1);
    expect(last?.kind === "message" && last.message.parts).toEqual([
      { text: "abcd", type: "text" },
    ]);
  });

  test("a tool call and its result travel as separate frames sharing a tool call id", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(
      SUBJECT,
      assistant(50, [
        { arguments: { command: "ls" }, id: "call-1", name: "bash", type: "toolCall" },
      ]),
      false
    );
    publisher.record(
      SUBJECT,
      {
        content: [{ text: "permission denied", type: "text" }],
        isError: true,
        role: "toolResult",
        timestamp: 60,
        toolCallId: "call-1",
        toolName: "bash",
      },
      false
    );

    const [call, result] = published;
    expect(call?.kind === "message" && call.message.parts).toEqual([
      {
        argsText: '{\n  "command": "ls"\n}',
        toolCallId: "call-1",
        toolName: "bash",
        type: "tool-call",
      },
    ]);
    expect(result?.kind === "tool-result" && result.result).toEqual({
      at: 60,
      isError: true,
      output: "permission denied",
      toolCallId: "call-1",
      toolName: "bash",
    });
  });

  test("a huge tool output is cut to the cap before it leaves the session", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(
      SUBJECT,
      {
        content: [{ text: "x".repeat(200_000), type: "text" }],
        role: "toolResult",
        timestamp: 60,
        toolCallId: "call-1",
        toolName: "read",
      },
      false
    );
    const frame = published[0];
    expect(frame?.kind === "tool-result" && frame.result.output).toBe(
      "x".repeat(AGENT_STREAM_LIMITS.toolChars) + AGENT_STREAM_TRUNCATION_SUFFIX
    );
  });

  test("replay keeps the newest frames inside its byte budget, oldest first", () => {
    const { publisher } = harness();
    const body = "y".repeat(AGENT_STREAM_LIMITS.partChars);
    for (let index = 0; index < 60; index += 1) {
      publisher.record(SUBJECT, assistant(index + 1, [{ text: body, type: "text" }]), false);
    }
    publisher.noteViewer();
    const replay = publisher.replay("s1");
    const bytes = JSON.stringify(replay.frames).length;
    expect(bytes).toBeLessThanOrEqual(AGENT_STREAM_LIMITS.historyBytes);
    expect(replay.frames.length).toBeGreaterThan(0);
    expect(replay.frames.length).toBeLessThan(60);
    const times = replay.frames.flatMap((frame) =>
      frame.kind === "message"
        ? [frame.message.at]
        : frame.kind === "tool-result"
          ? [frame.result.at]
          : []
    );
    expect(times).toEqual([...times].sort((left, right) => left - right));
    expect(times.at(-1)).toBe(60);
  });

  test("a message the host did not stamp carries no identity and is dropped", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    expect(publisher.record(SUBJECT, { content: "hi", role: "user" }, false)).toBe(false);
    expect(publisher.record(SUBJECT, "not a message", false)).toBe(false);
    expect(published).toEqual([]);
  });

  // The composer offers what the last commands frame lists: it must reach a viewer who opens the
  // session after the list was set, survive a full history, and change only when the list does.
  test("the session's commands are the last frame of a replay however long the history", () => {
    const { publisher } = harness();
    const body = "x".repeat(AGENT_STREAM_LIMITS.historyBytes / 10);
    for (let index = 0; index < 20; index += 1) {
      publisher.record(SUBJECT, assistant(index + 1, [{ text: body, type: "text" }]), false);
    }
    publisher.setCommands(SUBJECT, [{ name: "compact", source: "builtin" }]);
    publisher.noteViewer();

    const { frames } = publisher.replay("s1");
    expect(frames.at(-1)).toMatchObject({
      commands: [{ name: "compact", source: "builtin" }],
      kind: "commands",
    });
    expect(JSON.stringify(frames).length).toBeLessThanOrEqual(AGENT_STREAM_LIMITS.historyBytes);
  });

  test("a changed command list is published at once, and an unchanged one is not", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.setCommands(SUBJECT, [{ name: "compact", source: "builtin" }]);
    publisher.setCommands(SUBJECT, [{ name: "compact", source: "builtin" }]);
    publisher.setCommands(SUBJECT, [
      { name: "compact", source: "builtin" },
      { name: "skill:dispatch", source: "skill" },
    ]);

    expect(
      published.map((frame) => (frame.kind === "commands" ? frame.commands.length : -1))
    ).toEqual([1, 2]);
  });

  // A person types a command as one token, so a name they could not type is not offered, and a
  // name two sources share is offered once, as the first source lists it.
  test("a command list keeps each typable name once", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.setCommands(SUBJECT, [
      { name: "compact", source: "builtin", terminalOnly: undefined },
      { name: "two words", source: "prompt" },
      { name: "", source: "extension" },
      { name: "compact", source: "extension" },
      { name: "new", source: "builtin", terminalOnly: true },
    ]);

    const frame = published.at(-1);
    expect(frame?.kind === "commands" && frame.commands).toEqual([
      { name: "compact", source: "builtin" },
      { name: "new", source: "builtin", terminalOnly: true },
    ]);
  });
});

/**
 * Oh My Pi 18.3.2 hands `message_update` to extensions through its own queue while
 * `message_start` and `message_end` are emitted directly, so a message's last few updates arrive
 * after it has already settled — measured at about three per assistant message. Nothing below
 * may let one of those turn a finished turn back into a running one.
 */
describe("a message the host has settled", () => {
  test("is not reopened by the updates that arrive after its end", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(SUBJECT, assistant(50, [{ text: "Hello there", type: "text" }]), false);
    for (let index = 0; index < 3; index += 1) {
      const late = publisher.record(
        SUBJECT,
        assistant(50, [{ text: "Hello", type: "text" }]),
        true
      );
      expect(late).toBe(false);
    }

    // Nothing late reached the wire, and the replay a viewer opening this session gets shows
    // the turn finished. A streaming replay here is what disables assistant-ui's composer.
    expect(published).toHaveLength(1);
    const replayed = publisher.replay("s1").frames;
    expect(replayed).toHaveLength(1);
    const frame = replayed[0];
    expect(frame?.kind === "message" && frame.message.streaming).toBe(false);
    expect(frame?.kind === "message" && frame.message.parts).toEqual([
      { text: "Hello there", type: "text" },
    ]);
  });
});

describe("the session this publisher serves", () => {
  test("takes none of the previous session's conversation into the next one", () => {
    const { publisher } = harness();
    publisher.noteViewer();
    publisher.record(SUBJECT, assistant(50, [{ text: "the old session", type: "text" }]), false);
    expect(publisher.replay("s1").frames).toHaveLength(1);

    // What `/new`, `/resume`, a fork or a tree navigation does to the extension instance.
    publisher.reset();
    expect(publisher.watched).toBe(false);
    publisher.noteViewer();
    expect(publisher.replay("s2").frames).toEqual([]);
  });
});

describe("an unwatched session", () => {
  test("does not read a streaming message's content until someone asks for it", () => {
    const { publisher } = harness();
    let reads = 0;
    const parts = [{ text: "x".repeat(4_000), type: "text" }];
    // The host replaces a streaming message's content on every delta; this counts the reads a
    // delta costs. Building a frame per delta serialises the whole growing message each time,
    // which is quadratic in its size and was measured at 1.2 s of CPU for one 100 KB tool call.
    const message = {
      role: "assistant",
      timestamp: 50,
      get content() {
        reads += 1;
        return parts;
      },
    };
    for (let index = 0; index < 500; index += 1) {
      publisher.record(SUBJECT, message, true);
    }
    expect(reads).toBe(0);

    publisher.noteViewer();
    expect(publisher.replay("s1").frames).toHaveLength(1);
    expect(reads).toBe(1);
  });
});

describe("the replay budget", () => {
  test("is measured in the bytes NATS counts, not in characters", () => {
    const { publisher } = harness();
    // Box-drawing and CJK text is three UTF-8 bytes per character, so a character budget lets
    // through a reply three times its measured size — past the server's 1 MiB max_payload,
    // which the publish then throws on, killing the control pump with it.
    const wide = "\u2500\u4e2d".repeat(AGENT_STREAM_LIMITS.partChars / 2);
    for (let index = 0; index < 60; index += 1) {
      publisher.record(SUBJECT, assistant(index + 1, [{ text: wide, type: "text" }]), false);
    }
    publisher.noteViewer();
    const frames = publisher.replay("s1").frames;
    const bytes = new TextEncoder().encode(JSON.stringify(frames)).length;
    expect(bytes).toBeLessThanOrEqual(AGENT_STREAM_LIMITS.historyBytes);
    expect(frames.length).toBeGreaterThan(0);
  });
});

/** A user message as the host records one: a prompt's text part, stamped once. */
function user(timestamp: number, text: string) {
  return { content: [{ text, type: "text" }], role: "user", timestamp };
}

function dispatchIds(frames: readonly AgentStreamFrame[]): (string | undefined)[] {
  return frames.map((frame) =>
    frame.kind === "message" ? frame.message.dispatchMessageId : undefined
  );
}

/**
 * A person's direct message from Dispatch becomes the session's own user turn, and the viewer
 * also shows Dispatch's stored copy of it. Which user message that is, only the caller knows
 * (`matchInjectedUserTurn`), so it passes the id with the record, and every frame of that message
 * carries it.
 */
describe("a user turn a Dispatch message became", () => {
  test("carries the id it was recorded with on every later frame of that message and in the replay", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(SUBJECT, user(10, "Where is the dashboard?"), true, "m-1");
    publisher.record(SUBJECT, user(10, "Where is the dashboard?"), false);
    publisher.record(SUBJECT, user(30, "typed at the terminal"), false);
    publisher.record(SUBJECT, assistant(20, [{ text: "At /dash.", type: "text" }]), false);

    expect(dispatchIds(published)).toEqual(["m-1", "m-1", undefined, undefined]);
    expect(dispatchIds(publisher.replay("s1").frames)).toEqual(["m-1", undefined, undefined]);
  });

  test("goes with the conversation when the session is replaced", () => {
    const { published, publisher } = harness();
    publisher.record(SUBJECT, user(10, "ship it"), false, "m-1");
    publisher.reset();
    publisher.noteViewer();
    publisher.record(SUBJECT, user(10, "ship it"), false);

    expect(dispatchIds(published)).toEqual([undefined]);
  });
});

// The provider/model that produced an assistant turn (LEGION-548), read off the host's own
// assistant message, so the live view's header and transcript can show it without guessing.
describe("the model that produced an assistant turn", () => {
  test("combines provider and model from the host message onto the published frame", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(
      SUBJECT,
      {
        content: [{ text: "Hi", type: "text" }],
        model: "claude-opus-5",
        provider: "anthropic",
        role: "assistant",
        timestamp: 50,
      },
      false
    );
    const frame = published[0];
    expect(frame?.kind === "message" && frame.message.model).toBe("anthropic/claude-opus-5");
  });

  test("carries none when the host names only one of the pair", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(
      SUBJECT,
      { content: [], model: "claude-opus-5", role: "assistant", timestamp: 50 },
      false
    );
    const frame = published[0];
    expect(frame?.kind === "message" && frame.message.model).toBeUndefined();
  });

  test("never reaches a user message, even when its host object names one", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(
      SUBJECT,
      {
        content: "ship it",
        model: "claude-opus-5",
        provider: "anthropic",
        role: "user",
        timestamp: 10,
      },
      false
    );
    const frame = published[0];
    expect(frame?.kind === "message" && frame.message.model).toBeUndefined();
  });

  test("switches with the session's own model on the next turn", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(
      SUBJECT,
      {
        content: [{ text: "first", type: "text" }],
        model: "claude-opus-5",
        provider: "anthropic",
        role: "assistant",
        timestamp: 10,
      },
      false
    );
    publisher.record(
      SUBJECT,
      {
        content: [{ text: "second", type: "text" }],
        model: "claude-sonnet-5",
        provider: "anthropic",
        role: "assistant",
        timestamp: 20,
      },
      false
    );
    expect(published.map((frame) => frame.kind === "message" && frame.message.model)).toEqual([
      "anthropic/claude-opus-5",
      "anthropic/claude-sonnet-5",
    ]);
  });

  // An empty string still satisfies `typeof x === "string"`, so the type check alone would
  // combine it into a malformed `/claude-opus-5` or `anthropic/`.
  test("carries none when the host names an empty provider or an empty model", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    publisher.record(
      SUBJECT,
      { content: [], model: "claude-opus-5", provider: "", role: "assistant", timestamp: 50 },
      false
    );
    publisher.record(
      SUBJECT,
      { content: [], model: "", provider: "anthropic", role: "assistant", timestamp: 60 },
      false
    );
    expect(published.map((frame) => frame.kind === "message" && frame.message.model)).toEqual([
      undefined,
      undefined,
    ]);
  });

  test("caps an oversized combined model before it leaves the session", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    const oversized = "x".repeat(AGENT_STREAM_LIMITS.modelChars);
    publisher.record(
      SUBJECT,
      { content: [], model: oversized, provider: "anthropic", role: "assistant", timestamp: 50 },
      false
    );
    const frame = published[0];
    const model = frame?.kind === "message" ? frame.message.model : undefined;
    expect(model).toBe(
      `anthropic/${oversized}`.slice(0, AGENT_STREAM_LIMITS.modelChars) +
        AGENT_STREAM_TRUNCATION_SUFFIX
    );
  });
});
