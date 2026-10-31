import { expect, test } from "bun:test";
import {
  assistantRanTool,
  assistantText,
  LAST_MESSAGE_LIMIT,
  phaseStallEntryFrom,
  stepPhaseStall,
  turnEndOf,
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

// Oh My Pi stops a reply the provider refused with `error` and words it in `errorMessage`
// (`AssistantMessage` in `src/omp-host.d.ts`), which is how the turn that ends the run reads at
// `session_stop` and at `agent_end`.
test("a reply the provider refused classifies as the refusal, not as what the worker said", () => {
  expect(
    turnEndOf({
      ...assistant({ type: "text", text: "" }),
      stopReason: "error",
      errorMessage: "Overloaded: the model is busy",
    })
  ).toEqual({ kind: "refused", message: "Overloaded: the model is busy" });
  // A refusal the host could not word still has to read as a refusal.
  expect(turnEndOf({ ...assistant(), stopReason: "error" })).toEqual({
    kind: "refused",
    message: "error",
  });
  expect(
    turnEndOf({ ...assistant({ type: "text", text: "Pushed." }), stopReason: "stop" })
  ).toEqual({ kind: "settle", lastAssistantText: "Pushed." });
});

// Reminding a session the provider is refusing buys another refused turn, which is what left the
// implementer of dispatch://INFRA-264 reminded for ever.
test("a refused turn earns no reminder and moves nothing", () => {
  const open = { state: "open", count: 2 } as const;
  expect(stepPhaseStall(open, { kind: "refused", message: "Overloaded" })).toEqual({ state: open });
  const waiting = { state: "waiting", count: 1 } as const;
  expect(stepPhaseStall(waiting, { kind: "refused", message: "Overloaded" })).toEqual({
    state: waiting,
  });
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
