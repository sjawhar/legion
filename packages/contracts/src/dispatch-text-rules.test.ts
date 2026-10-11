import { describe, expect, test } from "bun:test";

import { isSessionId, pictureCaption, SESSION_ID_PATTERN } from "./dispatch-text-rules";

describe("isSessionId", () => {
  test("accepts what the server's text.IsSessionID accepts", () => {
    for (const value of [
      "01a1058e-f14f-7684-87eb-3dc885955551",
      "abc",
      "a.b_c~d",
      "émile",
      "日本",
    ]) {
      expect(isSessionId(value)).toBe(true);
    }
  });

  // The maintainability gate on #1800 found a third reader accepting `abc[1]` and `` abc`x ``,
  // which the server and the dashboard refuse: one rule, so no reader can be looser again.
  test("refuses the characters that structure or end a reference, every separator and a control", () => {
    for (const value of [
      "",
      "abc[1]",
      "abc`x",
      "a/b",
      "a?b",
      "a#b",
      "a b",
      "a\tb",
      "a\u00a0b",
      "a\ufeffb",
      "a\u0000b",
      "a<b",
      "a>b",
      'a"b',
      "a'b",
      "a]b",
      "a|b",
    ]) {
      expect(isSessionId(value)).toBe(false);
    }
  });

  test("embeds in a larger pattern under the u flag", () => {
    const reference = new RegExp(`^agent/(${SESSION_ID_PATTERN})/artifact/([a-z0-9-]+)$`, "u");
    expect(reference.exec("agent/01a1-abc/artifact/shot-png")?.[1]).toBe("01a1-abc");
    expect(reference.test("agent/01a1[abc]/artifact/shot-png")).toBe(false);
  });
});

describe("pictureCaption", () => {
  test("escapes what would end the caption or open a code span, and reads whitespace as a space", () => {
    expect(pictureCaption("shot [v2] `final`\\draft.png")).toBe(
      "shot \\[v2\\] \\`final\\`\\\\draft.png"
    );
    expect(pictureCaption("two\twords\r\nmore  here.png")).toBe("two words more here.png");
  });
});
