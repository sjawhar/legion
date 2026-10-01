import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";

import { composerReferences } from "./MentionComposer";

const origin = "https://dispatch.test";

// The dashboard's half of the shared text table: the composer's reference pills and unfurl cards
// cite exactly what the server's `text.Extract` indexes for the same body, so the dashboard and
// the reference graph never disagree about what a text cites. The Go reader walks the same rows.
test.each(
  DISPATCH_TEXT_REFERENCES.map((row) => [row.body, row.refs] as const)
)("%s cites %p", (body, refs) => {
  expect(composerReferences(body, origin).map((item) => item.reference)).toEqual([...refs]);
});

// A body near the server's 1 MiB request cap that trails a reference with a run of closing
// parentheses, alone or between the emphasis delimiters the trim also drops, still cites that
// reference, and a long run of closers followed by anything else is read in one pass too. The
// bound is generous for a loaded machine: a trim that rescans the run for every start, as an
// end-anchored regular expression does, takes over ten seconds on the 64 KiB body alone.
test.each([
  ["parentheses", `dispatch://CORE-1${")".repeat(1 << 20)}`, ["dispatch://CORE-1"]],
  [
    "parentheses and underscores",
    `dispatch://CORE-1${")_".repeat(1 << 19)}`,
    ["dispatch://CORE-1"],
  ],
  [
    "parentheses and asterisks after a dashboard URL",
    `${origin}/issues/CORE-1${")*".repeat(1 << 19)}`,
    ["dispatch://CORE-1"],
  ],
  ["parentheses then a letter", `dispatch://CORE-1${")".repeat(1 << 16)}x`, []],
] as const)("a reference trailed by %s is read in one pass", (_name, body, refs) => {
  const start = performance.now();
  const cited = composerReferences(body, origin).map((item) => item.reference);
  expect(performance.now() - start).toBeLessThan(2000);
  expect(cited).toEqual([...refs]);
});
