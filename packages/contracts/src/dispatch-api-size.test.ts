import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";

// Where the Dispatch API contract lives: `dispatch-api.ts`, the entry whose star exports every
// caller imports, and one module per area beside it. This file holds the bound on all of them.
const SOURCE = import.meta.dir;
const ENTRY = path.join(SOURCE, "dispatch-api.ts");
const MODULE_PREFIX = "dispatch-api-";

/**
 * How long `dispatch-api.ts` was before the split, counted as its text's lines rather than as its
 * newline count, so a file with a final newline counts the empty line after it: 4,329 lines,
 * every event payload in it written twice over, as a hand-written type and as a tolerant schema
 * with no derivation between them. The tests below assert the bound rather than today's lengths,
 * because a declaration moved between modules is a change to review, not a test to re-record.
 */
const FILE_LINES_BASE = 4329;

/** What the acceptance asks of the split: no module of it longer than this. */
const MODULE_BOUND = 1000;

describe("the size of the Dispatch API contract", () => {
  test("dispatch-api.ts is an entry no longer than the bound", () => {
    const lines = readFileSync(ENTRY, "utf8").split("\n").length;
    expect(
      lines,
      `dispatch-api.ts is ${lines} lines; it was ${FILE_LINES_BASE} before the split`
    ).toBeLessThanOrEqual(MODULE_BOUND);
  });

  // The split left 18 modules beside the entry, the longest of them `dispatch-api-delivery.ts`.
  test("no module of the split is longer than the bound", () => {
    const tooLong = readdirSync(SOURCE)
      .filter(
        (entry) =>
          entry.startsWith(MODULE_PREFIX) && entry.endsWith(".ts") && !entry.endsWith(".test.ts")
      )
      .map((entry) => ({
        file: entry,
        lines: readFileSync(path.join(SOURCE, entry), "utf8").split("\n").length,
      }))
      .filter(({ lines }) => lines > MODULE_BOUND);
    expect(tooLong).toEqual([]);
  });
});
