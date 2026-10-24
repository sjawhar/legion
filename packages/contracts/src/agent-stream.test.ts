import { expect, test } from "bun:test";

import {
  AGENT_STREAM_TRUNCATION_SUFFIX,
  capAgentStreamText,
  isTypableCommandName,
} from "./agent-stream";

// The one rule the session's publisher lists a command by and the page accepts one by: a name a
// person types after the slash, as one token.
test("a command name is one token a person can type", () => {
  expect(isTypableCommandName("compact")).toBe(true);
  expect(isTypableCommandName("skill:dispatch")).toBe(true);
  expect(isTypableCommandName("")).toBe(false);
  expect(isTypableCommandName(" ")).toBe(false);
  expect(isTypableCommandName("two words")).toBe(false);
  expect(isTypableCommandName("tab\there")).toBe(false);
  expect(isTypableCommandName("nul\u0000name")).toBe(false);
  expect(isTypableCommandName("del\u007f")).toBe(false);
  expect(isTypableCommandName("c1\u0085")).toBe(false);
});

// A cap counts UTF-16 units, and an emoji is two: cutting between them would send the first
// alone, which the page shows as U+FFFD, so the cut ends before the emoji.
test("a cap never cuts an emoji in half", () => {
  const capped = capAgentStreamText(`${"x".repeat(9)}😀 and more`, 10);
  expect(capped).toBe(`${"x".repeat(9)}${AGENT_STREAM_TRUNCATION_SUFFIX}`);
  expect(Buffer.from(capped).toString()).toBe(capped);
  expect(capAgentStreamText(`${"x".repeat(8)}😀 and more`, 10)).toBe(
    `${"x".repeat(8)}😀${AGENT_STREAM_TRUNCATION_SUFFIX}`
  );
});
