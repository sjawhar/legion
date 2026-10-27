import { expect, test } from "bun:test";
import {
  SESSION_ANSWERS_MARKER,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers";
import { withSessionAnswers } from "./session-answers-context";

// Message shapes as Oh My Pi hands them to a `context` handler.
const LIST = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
const list = { role: "user", content: [{ type: "text", text: LIST }], timestamp: 0 };
const skill = {
  role: "user",
  content: [{ type: "text", text: "<dispatch-first-skill>\nSearch Dispatch first." }],
  timestamp: 0,
};
const ask = { role: "user", content: [{ type: "text", text: "Fix the bug." }], timestamp: 1 };
const readCall = {
  role: "assistant",
  content: [{ type: "toolCall", id: "call-1", name: "read", arguments: { path: "x.ts" } }],
  timestamp: 2,
};
const readResult = {
  role: "toolResult",
  toolCallId: "call-1",
  toolName: "read",
  content: [{ type: "text", text: LIST }],
  isError: false,
  timestamp: 3,
};

/** A read that records each call and answers `text`. */
function reading(text: string | undefined): {
  readonly read: () => Promise<string | undefined>;
  readonly calls: () => number;
} {
  let calls = 0;
  return {
    read: async () => {
      calls += 1;
      return text;
    },
    calls: () => calls,
  };
}

test("the list goes after the request's last message, as a message of its own", async () => {
  const { read } = reading(LIST);
  expect(await withSessionAnswers([skill, ask], read)).toEqual([skill, ask, list]);
});

// The tool result quotes the marker: it is conversation, not a list another copy appended.
test("after a tool round the list follows the tool result, which keeps its own content", async () => {
  const { read } = reading(LIST);
  const messages = [skill, ask, readCall, readResult];
  expect(await withSessionAnswers(messages, read)).toEqual([...messages, list]);
});

test("the failure line goes where the list would", async () => {
  const line = sessionAnswersFailureLine("Dispatch did not answer within 800 ms");
  const { read } = reading(line);
  expect(await withSessionAnswers([ask], read)).toEqual([
    ask,
    { role: "user", content: [{ type: "text", text: line }], timestamp: 0 },
  ]);
});

test("no text to carry adds no message", async () => {
  const { read, calls } = reading(undefined);
  expect(await withSessionAnswers([skill, ask], read)).toBeUndefined();
  expect(calls()).toBe(1);
});

test("a request another copy of the extension already gave the list gets no second one, and no read", async () => {
  const { read, calls } = reading(LIST);
  expect(await withSessionAnswers([skill, ask, list], read)).toBeUndefined();
  const failure = {
    role: "user",
    content: [{ type: "text", text: sessionAnswersFailureLine("no route") }],
    timestamp: 0,
  };
  expect(await withSessionAnswers([ask, failure], read)).toBeUndefined();
  expect(calls()).toBe(0);
});

test("a list quoted earlier in the conversation does not switch the list off", async () => {
  const { read } = reading(LIST);
  const messages = [list, ask, readCall, readResult];
  expect(await withSessionAnswers(messages, read)).toEqual([...messages, list]);
});
