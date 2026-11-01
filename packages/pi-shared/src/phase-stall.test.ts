import { expect, test } from "bun:test";
import {
  assistantRanTool,
  assistantText,
  LAST_MESSAGE_LIMIT,
  phaseStallEntryFrom,
  REMINDER,
  recordedPhaseStall,
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
// start nothing until the daemon comes back to it. A reminder here would continue the answering
// turn in the same run: the work that continuation did would go with the pod, and the daemon
// would see no turn end.
test("a worker that answered the update task stands in awaiting-update, where a settle earns no reminder", () => {
  const answered = stepPhaseStall({ state: "open", count: 2 }, { kind: "update-answered" });
  expect(answered).toEqual({ state: { state: "awaiting-update", count: 0 } });
  expect(
    stepPhaseStall(answered.state, { kind: "settle", lastAssistantText: "Pushed and answered." })
  ).toEqual({ state: { state: "awaiting-update", count: 0 } });
  // The answering tool call itself, which the host reports from its own hook, leaves the wait as
  // it is, whichever of the two the host applies first.
  expect(stepPhaseStall(answered.state, { kind: "tool-call" })).toEqual({
    state: { state: "awaiting-update", count: 0 },
  });
  // What ends this wait is the daemon: the task it held behind the round, or the one saying the
  // round ended having replaced nothing, arrives as its own prompt and opens the phase again.
  expect(stepPhaseStall(answered.state, { kind: "assignment" })).toEqual({
    state: { state: "open", count: 0 },
  });
  // Anything else arriving from outside is something happening too, and the wait is over.
  expect(stepPhaseStall(answered.state, { kind: "inbound-event" })).toEqual({
    state: { state: "open", count: 0 },
  });
  // Nothing reminds into this wait, so `reminded` can only be a timer armed before the answer;
  // it is counted and the phase is open again, as it is out of a WAITING reply's wait.
  expect(stepPhaseStall(answered.state, { kind: "reminded" })).toEqual({
    state: { state: "open", count: 1 },
  });
});

// The record holds no `awaiting-update`: a record is read only by the process a relaunch starts,
// and that relaunch is the round the wait belonged to ending, so what is restored is the plain
// wait the reminder times out — and a build that knows only the three states still reads it.
test("awaiting-update is recorded as the waiting it is a kind of", () => {
  expect(recordedPhaseStall("awaiting-update")).toBe("waiting");
  for (const state of ["closed", "open", "waiting"] as const) {
    expect(recordedPhaseStall(state)).toBe(state);
  }
  expect(phaseStallEntryFrom({ state: "waiting", count: 0 })).toEqual({
    state: "waiting",
    count: 0,
    reported: false,
  });
});

// Answering is not completing, and a phase that is closed is not reopened by one: a worker
// between phases that answers is told nothing, and `handoff_complete` would refuse it anyway.
test("the update task's answer closes no phase, and reopens none", () => {
  expect(stepPhaseStall({ state: "closed", count: 0 }, { kind: "update-answered" })).toEqual({
    state: { state: "closed", count: 0 },
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
