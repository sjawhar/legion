import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";

import { composerReferences } from "./routes";

const origin = "https://dispatch.test";

// The dashboard's half of the shared text table: the composer's reference pills and unfurl cards
// cite exactly what the server's `text.Extract` indexes for the same body, so the dashboard and
// the reference graph never disagree about what a text cites. The Go reader walks the same rows.
// A row marked `grammar` holds a bare key, a `#id` or an id prefix, which the pills read through
// neither a `dispatch://` reference nor a dashboard address; `textReferences` walks those.
test.each(
  DISPATCH_TEXT_REFERENCES.filter((row) => row.grammar === undefined).map(
    (row) => [row.body, row.origin ?? origin, row.refs] as const
  )
)("%s cites, read against %s, %p", (body, server, refs) => {
  expect(composerReferences(body, server).map((item) => item.reference)).toEqual([...refs]);
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
  // A wall-clock budget, not one of the machine-sensitive waits this package's defaults now
  // cover (`web/src/__tests__/setup.ts`): the regression it guards, catastrophic backtracking,
  // costs minutes rather than milliseconds. These four rows read 0.08 s, 0.06 s, 0.04 s and
  // 0.00 s idle on a 2-core pod, and 0.14 s, 0.13 s, 0.10 s and 0.00 s loaded; 2000 ms is 12
  // times the slowest loaded reading of this change's measurements, so no loaded pod can fail
  // it by being slow.
  expect(performance.now() - start).toBeLessThan(2000);
  expect(cited).toEqual([...refs]);
});
