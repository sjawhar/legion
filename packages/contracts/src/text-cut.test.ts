import { expect, test } from "bun:test";

import { cutUnits } from "./text-cut";

// A limit counts UTF-16 units, and a character past U+FFFF (most emoji) is two of them, a
// surrogate pair. A cut between the two leaves the first alone, which UTF-8 cannot carry, so a
// reader is shown U+FFFD; the cut ends one unit short instead.
test("cuts to the limit in UTF-16 units, one unit short rather than through a surrogate pair", () => {
  const cases: [string, number, string][] = [
    ["abcdef", 3, "abc"],
    ["abc", 3, "abc"],
    ["abc", 10, "abc"],
    ["abc", Number.POSITIVE_INFINITY, "abc"],
    ["abc", 0, ""],
    ["ab😀cd", 2, "ab"],
    ["ab😀cd", 3, "ab"],
    ["ab😀cd", 4, "ab😀"],
    ["😀😀", 3, "😀"],
    ["😀", 1, ""],
  ];
  for (const [text, limit, cut] of cases) {
    expect([text, limit, cutUnits(text, limit)]).toEqual([text, limit, cut]);
    expect(Buffer.from(cut).toString()).toBe(cut);
  }
});
