import { expect, test } from "bun:test";
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";
import { withDispatchFirst } from "./dispatch-first";

// Message shapes as Oh My Pi hands them to a `context` handler.
const SKILL = `${DISPATCH_FIRST_MARKER}\nSearch Dispatch first.\n</dispatch-first-skill>`;
const skill = { role: "user", content: [{ type: "text", text: SKILL }], timestamp: 0 };
const ask = { role: "user", content: [{ type: "text", text: "Fix the bug." }], timestamp: 1 };
const quoting = `export const DISPATCH_FIRST_MARKER = "${DISPATCH_FIRST_MARKER}";`;
const readCall = {
  role: "assistant",
  content: [{ type: "toolCall", id: "call-1", name: "read", arguments: { path: "x.ts" } }],
  timestamp: 2,
};
const readResult = {
  role: "toolResult",
  toolCallId: "call-1",
  toolName: "read",
  content: [{ type: "text", text: quoting }],
  isError: false,
  timestamp: 3,
};

test("the first request gets the skill ahead of the conversation", () => {
  expect(withDispatchFirst([ask], SKILL)).toEqual([skill, ask]);
});

test("a tool result quoting the marker does not switch the skill off", () => {
  const messages = [ask, readCall, readResult];
  expect(withDispatchFirst(messages, SKILL)).toEqual([skill, ...messages]);
});

test("a delivered message quoting the marker does not switch the skill off", () => {
  const delivered = {
    role: "custom",
    customType: "envoy-message",
    content: `A reviewer wrote: ${SKILL}`,
    display: true,
    timestamp: 2,
  };
  expect(withDispatchFirst([ask, delivered], SKILL)).toEqual([skill, ask, delivered]);
});

test("the model's own reply quoting the marker does not switch the skill off", () => {
  const reply = { role: "assistant", content: [{ type: "text", text: quoting }], timestamp: 2 };
  expect(withDispatchFirst([ask, reply], SKILL)).toEqual([skill, ask, reply]);
});

test("a first user message that mentions the marker still gets the skill ahead of it", () => {
  const mention = {
    role: "user",
    content: [{ type: "text", text: `Why does ${DISPATCH_FIRST_MARKER} show up?` }],
    timestamp: 1,
  };
  expect(withDispatchFirst([mention], SKILL)).toEqual([skill, mention]);
});

test("a request another copy of the extension already gave the skill gets no second one", () => {
  expect(withDispatchFirst([skill, ask], SKILL)).toBeUndefined();
});

test("after a compaction the skill goes right after the summaries", () => {
  const summary = { role: "compactionSummary", summary: "Earlier work.", timestamp: 0 };
  expect(withDispatchFirst([summary, ask], SKILL)).toEqual([summary, skill, ask]);
  expect(withDispatchFirst([summary, skill, ask], SKILL)).toBeUndefined();
});
