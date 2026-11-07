import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { type ExportSurface, readExportSurface, SURFACE_ENTRIES } from "./export-surface";

/**
 * A branch may add to what `@legion/contracts` exports; it may not quietly change what a name
 * already means. A caller that parses a bus frame names a schema, and a name that was the
 * tolerant reading of that frame and became the exact one compiles at every call site and then
 * refuses frames it accepted the day before, saying nothing. So the surface of `main` is held
 * here as a committed snapshot - every exported name, what kind of thing it is, and for a schema
 * whether it refuses a key it does not declare - and the working tree is read the same way
 * (`export-surface.ts`) and compared against it. A name that is new in the working tree is free;
 * a name main exports must still be there, and must still read a frame as main read it.
 *
 * Regenerate the snapshot, after a rename main has taken, with:
 *
 *     cd packages/contracts && bun run src/export-surface.ts
 */

const SNAPSHOT = path.join(import.meta.dir, "export-surface.main.json");

const MAIN = JSON.parse(readFileSync(SNAPSHOT, "utf8")) as ExportSurface;

const HEAD = await readExportSurface(import.meta.dir);

for (const entry of SURFACE_ENTRIES) {
  test(`${entry} keeps every name main exports, and reads a frame as main does`, () => {
    const main = MAIN[entry] ?? {};
    const head = HEAD[entry] ?? {};
    const changed = Object.entries(main)
      .filter(([name, kind]) => head[name] !== kind)
      .map(([name, kind]) => `${name}: main ${kind}, head ${head[name] ?? "absent"}`);
    expect(
      changed,
      `regenerate ${path.basename(SNAPSHOT)} only for a rename main has taken`
    ).toEqual([]);
  });
}
