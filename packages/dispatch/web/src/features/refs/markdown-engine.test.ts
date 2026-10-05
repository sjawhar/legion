import { expect, spyOn, test } from "bun:test";

import serverBlockSchema from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import type { BlockSchema } from "../../api/types";
import * as schemaModule from "../doc/schema";
import { renderWithEngine } from "./markdown-engine";

const blockSchema = serverBlockSchema as unknown as BlockSchema;

/** Renders `markdown` through the shared engine as a surface would, and answers the parsed
 *  document's plain text, or `undefined` where the engine answered nothing for it. */
function parseThroughSharedEngine(markdown: string): Promise<string | undefined> {
  const { promise, resolve } = Promise.withResolvers<string | undefined>();
  renderWithEngine((engine) => {
    const doc = engine?.parse(markdown);
    resolve(doc === undefined ? undefined : doc.textBetween(0, doc.content.size, " ", " "));
  });
  return promise;
}

// LEGION-540. A body the schema has no node for throws, and that body falls back to its literal
// text. The throw must stop there: the next bodies through the same engine, on the same page,
// are unrelated text someone else wrote, and they render formatted. Before the headless editor
// dropped its refused parser, the one or two parses after a throw threw too, so a numbered list
// under a raw-HTML or mark-span turn in the live transcript read as `1. **Stop** …`. A span of a
// mark kind this schema does not carry still throws (reference-style links now resolve, so they
// no longer do), so it drives the recovery this test guards; removing the drop-on-throw in
// `@legion/proof-editor`'s headless parser fails the two assertions after the refused parse.
test("a parse the engine refuses leaves the next parses of unrelated text intact", async () => {
  const load = spyOn(schemaModule, "loadBlockSchema").mockResolvedValue(blockSchema);
  try {
    expect(await parseThroughSharedEngine("warm **up**")).toBe("warm up");
    // A span of a mark kind this schema has no parser for: the parser throws mid-paragraph.
    expect(
      await parseThroughSharedEngine('see <span data-dispatch="bogus">this</span> now')
    ).toBeUndefined();
    expect(await parseThroughSharedEngine("Fixed **two** bugs")).toBe("Fixed two bugs");
    expect(await parseThroughSharedEngine("1. one\n2. two")).toBe("one two");
    expect(await parseThroughSharedEngine("שלום **עולם** and مرحبا `code`")).toBe(
      "שלום עולם and مرحبا code"
    );
  } finally {
    load.mockRestore();
  }
});
