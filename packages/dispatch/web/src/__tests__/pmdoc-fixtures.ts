/**
 * The one markdown fixture table of this repository, read from where it lives rather than copied
 * into a test: `packages/envoy/internal/dispatch/pmdoc/testdata/corpus/*.md` and the
 * `testdata/fixtures.json` generated from them. The generator (`pmdoc/gen/gen.ts`) runs the
 * browser's own headless engine over each corpus file and records what it answers - the parsed
 * document, its Yjs update, the markdown it serializes back to, and the reading
 * (`@legion/proof-editor/reading`) a reader sees of it - so the Go reader's tests, these web unit
 * tests and the browser row of each parity family all compare against one artifact.
 *
 * `bun run check` in `packages/envoy/internal/dispatch/pmdoc/gen` is what keeps the table current:
 * it regenerates from the corpus and fails where the checked-in files differ, and CI runs it
 * (`ci.yaml`, the `envoy-go` job's "Check pmdoc fixtures are up to date"). A shape a test needs is
 * a new corpus file and a regenerated table, never a copy of its markdown in a test file.
 *
 * The table is 3.3 MB, so it is read and parsed once for the test process and selected by name.
 * The same cross-package read as `web/src/__tests__/setup.ts`'s and
 * `web/src/features/refs/markdown-engine.test.ts`'s of `pmdoc/schema/blocks.json`.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import type { DocumentReading } from "@legion/proof-editor/reading";

import type { DocumentJSON } from "../features/refs/markdown-parse.worker";

/** One corpus document and everything the engine answers for it. */
export interface PmdocFixture {
  /** The corpus file's name without its `.md`. */
  readonly name: string;
  /** The corpus file's bytes. */
  readonly markdown: string;
  /** The parsed document, as `ProseMirrorNode.toJSON()` writes it. */
  readonly pm_json: DocumentJSON;
  /** The parsed document as one Yjs update, base64: what the Go reader decodes. */
  readonly yjs_update_v1_b64: string;
  /** The markdown the engine's serializer writes the parsed document back as. */
  readonly rendered_by_milkdown: string;
  /** What a reader sees of the parsed document: each paragraph's text with the text of its `em`,
   *  `strong` and `del` elements, and each table's cells. */
  readonly reading: DocumentReading;
}

const TABLE_PATH = fileURLToPath(
  new URL("../../../../envoy/internal/dispatch/pmdoc/testdata/fixtures.json", import.meta.url)
);

/** Every entry of the table, in the generator's order (the corpus file names, sorted). */
export const pmdocFixtures: readonly PmdocFixture[] = JSON.parse(readFileSync(TABLE_PATH, "utf8"));

const byName = new Map(pmdocFixtures.map((entry) => [entry.name, entry]));

/** The entry `name`, which is a corpus file's name without its `.md`. */
export function fixture(name: string): PmdocFixture {
  const entry = byName.get(name);
  if (entry === undefined) {
    throw new Error(
      `no fixture named "${name}" in ${TABLE_PATH}: write ` +
        `packages/envoy/internal/dispatch/pmdoc/testdata/corpus/${name}.md and regenerate the ` +
        `table with \`bun run gen\` in packages/envoy/internal/dispatch/pmdoc/gen`
    );
  }
  return entry;
}
