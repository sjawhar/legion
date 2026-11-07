import { describe, expect, test } from "bun:test";

import {
  comparableLines,
  isSessionId,
  MAX_REFERENCE_TARGET_BYTES,
  pictureCaption,
  pictureSyntax,
  referenceTargetFits,
  SESSION_ID_PATTERN,
  trimmedLines,
  utf8Bytes,
} from "./dispatch-text-rules";

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

// A string's UTF-8 bytes as Go's `len` counts them: a multi-byte character in its bytes wherever
// it stands, a Latin-1 one ending a run past 1024 characters too (Bun 1.3's TextEncoder writes that
// one as a single NUL byte), and a lone surrogate as the three of the U+FFFD encoding writes.
test.each([
  ["", 0],
  ["ship-it", 7],
  ["é", 2],
  ["日", 3],
  ["📊", 4],
  [`${"a".repeat(2000)}é`, 2002],
  ["é".repeat(1500), 3000],
  [`${"a".repeat(1024)}é${"b".repeat(10)}`, 1036],
  ["\ud800", 3],
  ["x\udc00y", 5],
] as const)("counts the UTF-8 bytes of %p", (value, bytes) => {
  expect(utf8Bytes(value)).toBe(bytes);
});

// A target id fits at MAX_REFERENCE_TARGET_BYTES UTF-8 bytes and not one byte past, a multi-byte
// character counted in its bytes whether it fills the target or ends a long ASCII run: `é` (two
// bytes, one UTF-16 unit), `日` (three, one) or `📊` (four, two).
test.each([
  ["CORE-1/", (room: number) => "é".repeat(Math.floor(room / 2)) + "x".repeat(room % 2)],
  ["CORE/", (room: number) => "日".repeat(Math.floor(room / 3)) + "x".repeat(room % 3)],
  ["CORE-1/spec#", (room: number) => `${"x".repeat(room - 2)}é`],
  ["CORE/handbook-md#", (room: number) => `${"x".repeat(room - 4)}📊`],
] as const)("bounds a target id starting %s", (prefix, runOf) => {
  const target = prefix + runOf(MAX_REFERENCE_TARGET_BYTES - prefix.length);
  expect(referenceTargetFits(target)).toBe(true);
  expect(referenceTargetFits(`${target}x`)).toBe(false);
});

describe("pictureCaption", () => {
  test("escapes what would end the caption or open a code span, and reads whitespace as a space", () => {
    expect(pictureCaption("shot [v2] `final`\\draft.png")).toBe(
      "shot \\[v2\\] \\`final\\`\\\\draft.png"
    );
    expect(pictureCaption("two\twords\r\nmore  here.png")).toBe("two words more here.png");
  });
});

// The caption and title bounds, which keep a reader from rescanning a body of many `![` from
// every one of them: a picture written the ordinary way still reads, and one whose caption or
// title runs past its bound is no picture at all.
describe("pictureSyntax", () => {
  const address = "dispatch://LEGION-1/artifact/shot-png@v1";

  test("reads the pictures a text shows, each address its first group", () => {
    const text = `Here ![shot.png](${address}) and ![a\\]b.png](${address} "the shot").`;
    expect([...text.matchAll(pictureSyntax())].map((match) => match[1])).toEqual([
      address,
      address,
    ]);
  });

  test("reads a caption and a title that fill their bound", () => {
    const text = `![${"c".repeat(512)}](${address} "${"t".repeat(256)}")`;
    expect([...text.matchAll(pictureSyntax())].map((match) => match[1])).toEqual([address]);
  });

  test("reads no picture whose caption or title runs past its bound", () => {
    const longCaption = `![${"c".repeat(513)}](${address})`;
    const longTitle = `![shot.png](${address} "${"t".repeat(257)}")`;
    expect([...longCaption.matchAll(pictureSyntax())]).toEqual([]);
    expect([...longTitle.matchAll(pictureSyntax())]).toEqual([]);
  });

  // A line break ends a picture in markdown, so a caption that crosses one is text, not a
  // caption: the picture on the second line still reads.
  test("reads no caption across a line break", () => {
    const text = `![shot\n.png](${address})\n![two.png](${address})`;
    expect([...text.matchAll(pictureSyntax())].map((match) => match[1])).toEqual([address]);
  });
});

// The one rule the Claude Code plugin tags a streamed row by and the Agents page reads a stored
// body by, so a person's message shows once (INFRA-1026).
describe("comparableLines", () => {
  const address = "dispatch://agent/s1/artifact/i3-blue96-png@v1";

  test("takes out the pictures, trims each line and drops the empty ones", () => {
    expect(comparableLines(`  Look at this ![blue](${address})  \n\n  and this  \n`)).toEqual([
      "Look at this",
      "and this",
    ]);
  });

  test("leaves a line that is a bracketed word of the person's own", () => {
    expect(comparableLines(`[ack]\n\n![blue](${address})`)).toEqual(["[ack]"]);
  });

  test("leaves text that only looks like a picture, which is no picture at all", () => {
    expect(comparableLines("![shot.png](https://example.test/shot.png)")).toEqual([
      "![shot.png](https://example.test/shot.png)",
    ]);
  });

  test("is the exported line step applied to the text its pictures are out of", () => {
    const text = `  Look at this ![blue](${address})  \n\n  and this  \n`;
    expect(comparableLines(text)).toEqual(trimmedLines(text.replace(pictureSyntax(), "")));
  });
});

// The line step both sides of the Agents page's comparison share: the body's through
// `comparableLines`, the streamed row's through `rowLines`, which strips the row's pictures its
// own way first (INFRA-1026).
describe("trimmedLines", () => {
  const address = "dispatch://agent/s1/artifact/i3-blue96-png@v1";

  test("trims each line and drops the empty ones, whatever the line endings", () => {
    expect(trimmedLines("  Look at this  \r\n\r\n\tand this\n")).toEqual([
      "Look at this",
      "and this",
    ]);
  });

  test("takes no picture out: that step is `comparableLines`'s own", () => {
    expect(trimmedLines(`Look at this ![blue](${address})`)).toEqual([
      `Look at this ![blue](${address})`,
    ]);
  });
});
