import { describe, expect, test } from "bun:test";
import { unionByNumber } from "../github.ts";

describe("unionByNumber", () => {
  test("takes each PR of either list once, sorted by number", () => {
    const updated = [
      { number: 30, headRefName: "legion/C" },
      { number: 10, headRefName: "legion/A" },
    ];
    const open = [
      { number: 20, headRefName: "legion/B" },
      { number: 30, headRefName: "legion/C" },
      { number: 5, headRefName: "other" },
    ];
    expect(unionByNumber(updated, open)).toEqual([
      { number: 5, headRefName: "other" },
      { number: 10, headRefName: "legion/A" },
      { number: 20, headRefName: "legion/B" },
      { number: 30, headRefName: "legion/C" },
    ]);
  });

  test("an empty list adds nothing", () => {
    expect(unionByNumber([{ number: 1, headRefName: "a" }], [])).toEqual([
      { number: 1, headRefName: "a" },
    ]);
    expect(unionByNumber([], [])).toEqual([]);
  });
});
