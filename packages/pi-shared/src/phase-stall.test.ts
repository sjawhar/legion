import { expect, test } from "bun:test";
import {
  assistantRanTool,
  assistantText,
  LAST_MESSAGE_LIMIT,
  phaseStallEntryFrom,
  REMINDER,
  stepPhaseStall,
} from "./phase-stall";

const assistant = (...content: unknown[]) => ({ role: "assistant", content });

// Oh My Pi's `message_end` hands `toolCall` (the shape packages/pi-envoy/src/agent-stream.ts
// reads); Claude Code names it `tool_use`. Matching one name only once had a working Oh My Pi
// worker reported to its architect as stalled.
test("a tool call under either host's block name is a tool call the turn ran", () => {
  expect(
    assistantRanTool(
      assistant(
        { type: "text", text: "Reading the file." },
        { type: "toolCall", id: "call_1", name: "read", arguments: { path: "main.py" } }
      )
    )
  ).toBe(true);
  expect(
    assistantRanTool(
      assistant({ type: "tool_use", id: "toolu_1", name: "read", input: { path: "main.py" } })
    )
  ).toBe(true);
});

test("a message with no tool-call block, or one that is not the assistant's, ran no tool", () => {
  expect(assistantRanTool(assistant({ type: "text", text: "Done." }))).toBe(false);
  expect(assistantRanTool(assistant({ type: "thinking", thinking: "Hmm." }))).toBe(false);
  expect(
    assistantRanTool({
      role: "user",
      content: [{ type: "toolCall", id: "call_1", name: "read", arguments: {} }],
    })
  ).toBe(false);
  expect(assistantRanTool(undefined)).toBe(false);
});

test("an assistant message's text is its text blocks joined, and nothing of its tool calls", () => {
  expect(
    assistantText(
      assistant(
        { type: "text", text: "Pushed." },
        { type: "toolCall", id: "call_1", name: "bash", arguments: { command: "ls" } },
        { type: "text", text: "WAITING: CI." }
      )
    )
  ).toBe("Pushed.\n\nWAITING: CI.");
});

test("a record an earlier build wrote, with no last message, still reads", () => {
  expect(phaseStallEntryFrom({ state: "open", count: 2, reported: false })).toEqual({
    state: "open",
    count: 2,
    reported: false,
  });
});

test("a record's last message reads back no longer than the notice quotes", () => {
  expect(
    phaseStallEntryFrom({ state: "open", count: 1, reported: false, lastMessage: "Pushed." })
  ).toEqual({ state: "open", count: 1, reported: false, lastMessage: "Pushed." });
  const long = "x".repeat(LAST_MESSAGE_LIMIT + 40);
  expect(phaseStallEntryFrom({ state: "open", count: 1, lastMessage: long })?.lastMessage).toBe(
    long.slice(0, LAST_MESSAGE_LIMIT)
  );
  expect(phaseStallEntryFrom({ state: "open", count: 1, lastMessage: 7 })).toEqual({
    state: "open",
    count: 1,
    reported: false,
  });
});

// A worker whose turn ends while its own background job runs is not stalled: that job's
// result starts the next turn, and a reminder in between would be telling a working worker that
// it has done nothing. It buys the same quiet wait a WAITING line does, which is what bounds it:
// a job that never finishes is a wait the host's waiting delay ends.
test("a settle with the worker's own background work running earns no reminder", () => {
  expect(
    stepPhaseStall(
      { state: "open", count: 2 },
      { kind: "settle", lastAssistantText: "Started the build.", backgroundWork: true }
    )
  ).toEqual({ state: { state: "waiting", count: 2 } });
});

test("a settle with no background work left running is reminded as it always was", () => {
  for (const backgroundWork of [false, undefined]) {
    expect(
      stepPhaseStall(
        { state: "open", count: 2 },
        { kind: "settle", lastAssistantText: "Started the build.", backgroundWork }
      )
    ).toEqual({ state: { state: "open", count: 2 }, reminder: REMINDER });
  }
});

test("background work reopens nothing: a closed phase settles closed, and the wait ends on a reminder", () => {
  expect(
    stepPhaseStall(
      { state: "closed", count: 0 },
      { kind: "settle", lastAssistantText: "Done.", backgroundWork: true }
    )
  ).toEqual({ state: { state: "closed", count: 0 } });
  expect(stepPhaseStall({ state: "waiting", count: 2 }, { kind: "reminded" })).toEqual({
    state: { state: "open", count: 3 },
  });
});
