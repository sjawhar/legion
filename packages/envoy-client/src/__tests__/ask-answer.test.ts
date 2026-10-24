import { describe, expect, test } from "bun:test";
import { textHead } from "../ask-answer";

describe("textHead", () => {
  // The head counts UTF-16 units, and an emoji is two: a cut between them would leave the first
  // alone, which a host prints as U+FFFD, so the head stops before the emoji.
  test("cuts a long text to 120 units with an ellipsis, never through an emoji", () => {
    const split = textHead(`${"a".repeat(119)}😀 and more`);
    expect(split).toBe(`${"a".repeat(119)}…`);
    expect(Buffer.from(split).toString()).toBe(split);
    expect(textHead(`${"a".repeat(118)}😀 and more`)).toBe(`${"a".repeat(118)}😀…`);
    expect(textHead(`${"a".repeat(118)}😀`)).toBe(`${"a".repeat(118)}😀`);
  });
});
