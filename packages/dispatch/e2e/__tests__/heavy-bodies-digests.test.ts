import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { heavyBodies } from "../heavy-bodies";

// The heavy bodies are spelled twice: here, for the browser suite to draw
// (`e2e/heavy-bodies.ts`), and in Go, for the document writes the API gates
// (`packages/envoy/internal/dispatch/pmdoc/heavybodies`). The digest table below is what holds
// the two spellings to each other - a body respelled on one side alone fails that side - and Go
// reads the same file in `heavybodies/digests_test.go` (INFRA-1404). The same cross-package read
// as `e2e/engine-reading.ts`'s and `web/src/__tests__/pmdoc-fixtures.ts`'s of pmdoc's own files.

/** One body both sides spell, with the length and digest neither may leave. */
interface SharedRow {
  readonly id: string;
  readonly goName: string;
  readonly typescriptExport: string;
  readonly length: number;
  readonly sha256: string;
}

/** One body only one side spells, and why the other does not. */
interface OneSideRow {
  readonly id: string;
  readonly side: "go" | "typescript";
  readonly why: string;
}

/** `shared-bodies.json` whole: its own contract, and the two lists. */
interface DigestTable {
  readonly schema: string;
  // `test.each` takes a mutable table, so these are not `readonly`.
  readonly shared: SharedRow[];
  readonly oneSideOnly: OneSideRow[];
}

const TABLE_PATH = fileURLToPath(
  new URL(
    "../../../envoy/internal/dispatch/pmdoc/heavybodies/testdata/shared-bodies.json",
    import.meta.url
  )
);
const table = JSON.parse(readFileSync(TABLE_PATH, "utf8")) as DigestTable;

describe(`the bodies ${TABLE_PATH} shares with Go`, () => {
  test("the table shares some body, and says what it is", () => {
    expect(table.schema).not.toBe("");
    expect(table.shared.length).toBeGreaterThan(0);
  });

  test.each(table.shared)("$typescriptExport is the bytes Go's $goName spells", (row) => {
    const body = heavyBodies[row.id];
    if (body === undefined) {
      throw new Error(
        `${TABLE_PATH} shares "${row.id}" (the Go shape "${row.goName}"), which heavy-bodies.ts ` +
          `does not spell: export the body again under that id, or move its row to oneSideOnly ` +
          `with why Go alone has it`
      );
    }
    const digest = createHash("sha256").update(body, "utf8").digest("hex");
    expect(`${row.id}: ${body.length} characters, sha256 ${digest}`).toBe(
      `${row.id}: ${row.length} characters, sha256 ${row.sha256}`
    );
  });
});

describe(`what ${TABLE_PATH} says this side spells`, () => {
  test("every body here stands in the table once, and every row for this side is spelled here", () => {
    const listed = new Map<string, string>();
    for (const row of [
      ...table.shared.map((row) => ({ id: row.id, list: "shared" })),
      ...table.oneSideOnly.map((row) => ({ id: row.id, list: `oneSideOnly (${row.side})` })),
    ]) {
      const first = listed.get(row.id);
      if (first !== undefined) {
        throw new Error(`${TABLE_PATH} names "${row.id}" twice, in ${first} and in ${row.list}`);
      }
      listed.set(row.id, row.list);
    }
    const theirs = [...listed]
      .filter(([, list]) => list !== "oneSideOnly (go)")
      .map(([id]) => id)
      .sort();
    expect(Object.keys(heavyBodies).sort()).toEqual(theirs);
  });

  test.each(table.oneSideOnly)("$id is $side's alone, and the table says why", (row) => {
    expect(["go", "typescript"]).toContain(row.side);
    expect(row.why).not.toBe("");
  });
});
