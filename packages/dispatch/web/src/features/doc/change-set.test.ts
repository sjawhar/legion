import { expect, test } from "bun:test";

import { changedWords, pairBlocks } from "./change-set";

test("two equal block lists pair up as unchanged", () => {
  expect(pairBlocks(["# Plan", "One block."], ["# Plan", "One block."])).toEqual([
    { kind: "unchanged", before: 0, after: 0 },
    { kind: "unchanged", before: 1, after: 1 },
  ]);
});

test("a block that changed pairs the removed one with the added one at that place", () => {
  expect(
    pairBlocks(
      ["# Plan", "It ships behind a flag.", "The end."],
      ["# Plan", "It ships behind a canary.", "The end."]
    )
  ).toEqual([
    { kind: "unchanged", before: 0, after: 0 },
    { kind: "changed", before: 1, after: 1 },
    { kind: "unchanged", before: 2, after: 2 },
  ]);
});

test("a longer removed side leaves its remainder removed, and a longer added side added", () => {
  expect(
    pairBlocks(["# Plan", "Gone one.", "Gone two.", "The end."], ["# Plan", "The end."])
  ).toEqual([
    { kind: "unchanged", before: 0, after: 0 },
    { kind: "removed", before: 1, after: undefined },
    { kind: "removed", before: 2, after: undefined },
    { kind: "unchanged", before: 3, after: 1 },
  ]);

  expect(pairBlocks(["# Plan"], ["# Plan", "New one.", "New two."])).toEqual([
    { kind: "unchanged", before: 0, after: 0 },
    { kind: "added", before: undefined, after: 1 },
    { kind: "added", before: undefined, after: 2 },
  ]);
});

test("the word parts of a changed pair name the removed and the added words and leave the rest", () => {
  expect(
    changedWords("The plan ships behind a flag today.", "The plan ships behind a canary today.")
  ).toEqual([
    { kind: "same", value: "The plan ships behind a " },
    { kind: "removed", value: "flag" },
    { kind: "added", value: "canary" },
    { kind: "same", value: " today." },
  ]);
});

test("a pair too far apart to diff is the whole block removed and the whole block added", () => {
  // Two unrelated blocks of 600 words - one list, one table, one fenced code block replaced by
  // another - share no word in order, so their word diff is thousands of edits: past the bound,
  // and seconds of the main thread if nothing bounded it.
  const before = Array.from({ length: 600 }, (_, index) => `before${index}`).join(" ");
  const after = Array.from({ length: 600 }, (_, index) => `after${index}`).join(" ");

  expect(changedWords(before, after)).toEqual([
    { kind: "removed", value: before },
    { kind: "added", value: after },
  ]);
});
