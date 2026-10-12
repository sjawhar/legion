import { expect, test } from "bun:test";

import { isTypableCommandName } from "./agent-stream";

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
