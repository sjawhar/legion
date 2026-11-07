import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import * as heavyBodiesModule from "../heavy-bodies";

// The heavy bodies are spelled twice: here, for the browser suite to draw
// (`e2e/heavy-bodies.ts`), and in Go, for the document writes the API gates
// (`packages/envoy/internal/dispatch/pmdoc/heavybodies`). The digest table below is what holds
// the two spellings to each other - a body respelled on one side alone fails that side - and Go
// reads the same file in `heavybodies/digests_test.go` (INFRA-1404). The same cross-package read
// as `e2e/engine-reading.ts`'s and `web/src/__tests__/pmdoc-fixtures.ts`'s of pmdoc's own files.
//
// What this side is walked over is the module whole, not the `heavyBodies` record alone: every
// string export of `heavy-bodies.ts` is a body and must stand in that record, so a body exported
// and left out of it fails here rather than passing unmentioned.

/** One body both sides spell, with the length and digest neither may leave. */
interface SharedRow {
  readonly id: string;
  readonly goName: string;
  readonly typescriptExport: string;
  /** The body's length in bytes, UTF-8, as Go's `len` over the same string counts it. */
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

const { heavyBodies } = heavyBodiesModule;

/** Every body `heavy-bodies.ts` exports, by the name it exports it under: its string exports,
 *  which are its bodies - `padTo` and `nestedByIndentation` are functions, and `heavyBodies` is
 *  the record of the bodies rather than one of them. */
const exportedBodies = new Map(
  Object.entries(heavyBodiesModule).filter(
    (entry): entry is [string, string] => typeof entry[1] === "string"
  )
);

describe(`the bodies ${TABLE_PATH} shares with Go`, () => {
  test("the table shares some body, and this module exports some", () => {
    expect(table.schema).not.toBe("");
    expect(table.shared.length).toBeGreaterThan(0);
    expect(exportedBodies.size).toBeGreaterThan(0);
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
    const exported = exportedBodies.get(row.typescriptExport);
    if (exported === undefined) {
      throw new Error(
        `${TABLE_PATH} has "${row.id}" as heavy-bodies.ts's ${row.typescriptExport}, which that ` +
          `module exports no body under: name the export the body is spelled as now, or export ` +
          `it under that name again`
      );
    }
    if (exported !== body) {
      throw new Error(
        `heavy-bodies.ts's ${row.typescriptExport} is not the body its heavyBodies record holds ` +
          `under "${row.id}": ${TABLE_PATH} names one export for that id, so the two must be ` +
          `the same body`
      );
    }
    const digest = createHash("sha256").update(body, "utf8").digest("hex");
    expect(`${row.id}: ${Buffer.byteLength(body, "utf8")} bytes, sha256 ${digest}`).toBe(
      `${row.id}: ${row.length} bytes, sha256 ${row.sha256}`
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

  // The walk that makes this side total. The record above is hand-kept, so a body exported and
  // left out of it would stand in no list of the table and nothing would say so: this reads the
  // module itself and holds its string exports and the record to each other by their bytes,
  // which is all either side can be matched on - a string carries no identity here, so bodies
  // spelled alike are one body to this walk. Four things fail it: an export whose bytes no
  // entry of the record holds, a record entry whose bytes no export holds, one body exported
  // under two names, and one body recorded under two ids. The last two are what make the match
  // one for one - two names, or two ids, standing on one body would otherwise collapse into a
  // single entry and leave the walk nothing to notice.
  test("every body this module exports stands in its heavyBodies record, and the other way", () => {
    const idsByBody = new Map<string, string>();
    const collidingIds: string[] = [];
    for (const [id, body] of Object.entries(heavyBodies)) {
      const twin = idsByBody.get(body);
      if (twin === undefined) {
        idsByBody.set(body, id);
      } else {
        collidingIds.push(`${twin} and ${id}`);
      }
    }
    if (idsByBody.size !== Object.keys(heavyBodies).length) {
      throw new Error(
        `heavy-bodies.ts's heavyBodies record holds one body under two ids, ` +
          `${collidingIds.join(", ")}: each body stands under one id here, so that one row of ` +
          `${TABLE_PATH} pins it - drop the second id, or spell the body it is meant to hold`
      );
    }
    const namesByBody = new Map<string, string>();
    for (const [name, body] of exportedBodies) {
      const twin = namesByBody.get(body);
      if (twin !== undefined) {
        throw new Error(
          `heavy-bodies.ts exports one body as both ${twin} and ${name}: each body is spelled ` +
            `once here, so that one row of ${TABLE_PATH} pins it`
        );
      }
      namesByBody.set(body, name);
    }
    const exportedAndUnrecorded = [...exportedBodies]
      .filter(([, body]) => !idsByBody.has(body))
      .map(([name]) => name);
    const recordedAndUnexported = Object.entries(heavyBodies)
      .filter(([, body]) => !namesByBody.has(body))
      .map(([id]) => id);
    // An export left out: put it in the record under the id the table pins it to, shared with Go
    // or oneSideOnly as this side's own. A record entry whose bytes no export holds: spell that
    // body as an export and hold the record to it, so a spec and the table read the same body.
    expect({ exportedAndUnrecorded, recordedAndUnexported }).toEqual({
      exportedAndUnrecorded: [],
      recordedAndUnexported: [],
    });
  });

  test.each(table.oneSideOnly)("$id is $side's alone, and the table says why", (row) => {
    expect(["go", "typescript"]).toContain(row.side);
    expect(row.why).not.toBe("");
  });
});
