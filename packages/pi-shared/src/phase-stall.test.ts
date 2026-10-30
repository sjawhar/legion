import { expect, test } from "bun:test";
import {
  assistantRanTool,
  assistantText,
  LAST_MESSAGE_LIMIT,
  phaseStallEntryFrom,
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

// INFRA-484: the worker has answered the daemon's update task (`ready_for_update`), so it is to
// start nothing until the process its claim runs in comes back. A reminder here would continue
// the answering turn in the same run: the work that continuation did would go with the pod, and
// the daemon would see no turn end.
test("a worker that answered the update task waits: the phase stays open and the settle earns no reminder", () => {
  const answered = stepPhaseStall({ state: "open", count: 2 }, { kind: "update-answered" });
  expect(answered).toEqual({ state: { state: "waiting", count: 0 } });
  expect(
    stepPhaseStall(answered.state, { kind: "settle", lastAssistantText: "Pushed and answered." })
  ).toEqual({ state: { state: "waiting", count: 0 } });
  // The answering tool call itself, which the host reports from its own hook, leaves the wait as
  // it is, whichever of the two the host applies first.
  expect(stepPhaseStall(answered.state, { kind: "tool-call" })).toEqual({
    state: { state: "waiting", count: 0 },
  });
  // The reminding never stops: the quiet wait's own reminder ends the wait, as a WAITING reply's
  // does, so a replacement that never comes is asked about again.
  expect(stepPhaseStall(answered.state, { kind: "reminded" })).toEqual({
    state: { state: "open", count: 1 },
  });
  // Whatever arrives from outside is something happening, and the wait is over.
  expect(stepPhaseStall(answered.state, { kind: "inbound-event" })).toEqual({
    state: { state: "open", count: 0 },
  });
});

// Answering is not completing, and a phase that is closed is not reopened by one: a worker
// between phases that answers is told nothing, and `handoff_complete` would refuse it anyway.
test("the update task's answer closes no phase, and reopens none", () => {
  expect(stepPhaseStall({ state: "closed", count: 0 }, { kind: "update-answered" })).toEqual({
    state: { state: "closed", count: 0 },
  });
});
