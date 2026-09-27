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
    const times = replay.frames.map((frame) =>
      frame.kind === "message" ? frame.message.at : frame.result.at
    );
    expect(times).toEqual([...times].sort((left, right) => left - right));
    expect(times.at(-1)).toBe(60);
  });

  test("a message the host did not stamp carries no identity and is dropped", () => {
    const { published, publisher } = harness();
    publisher.noteViewer();
    expect(publisher.record(SUBJECT, { content: "hi", role: "user" }, false)).toBeNull();
    expect(publisher.record(SUBJECT, "not a message", false)).toBeNull();
    expect(published).toEqual([]);
  });
});
