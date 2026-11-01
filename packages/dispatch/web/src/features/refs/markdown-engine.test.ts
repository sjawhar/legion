import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "prosemirror-model";

import manifest from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import { renderWithEngine } from "./markdown-engine";

/** Renders `markdown` through the shared engine as a surface would, and answers the parsed
 *  document, or `undefined` where the engine answered nothing for it. */
function parsedThroughSharedEngine(markdown: string): Promise<ProseMirrorNode | undefined> {
  const { promise, resolve } = Promise.withResolvers<ProseMirrorNode | undefined>();
  renderWithEngine((engine) => resolve(engine?.parse(markdown)));
  return promise;
}

/** The plain text of `markdown` parsed through the shared engine, or `undefined` where the engine
 *  answered nothing for it. */
async function parseThroughSharedEngine(markdown: string): Promise<string | undefined> {
  const doc = await parsedThroughSharedEngine(markdown);
  return doc === undefined ? undefined : doc.textBetween(0, doc.content.size, " ", " ");
}

// During a release the process that answers the page can be of the other build, and its schema
// route names that build's manifest. The engine parses with the manifest this page was built
// with, so a typed block that manifest declares reads as that block whatever the route would say,
// and the engine asks no server for a schema.
test("the engine parses with the manifest the page was built with and reads none from the server", async () => {
  const realFetch = globalThis.fetch;
  const schemaReads: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = input.toString();
    if (path.startsWith("/api/v1/schema/blocks")) {
      schemaReads.push(path);
      return Response.json({ ...manifest, types: [], version: manifest.version + 1 });
    }
    return realFetch(input, init);
  }) as unknown as typeof fetch;

  try {
    const doc = await parsedThroughSharedEngine(':::callout{kind="warning"}\nRead this.\n:::\n');
    expect(doc?.firstChild?.type.name).toBe("callout");
    expect(doc?.firstChild?.attrs.kind).toBe("warning");
    expect(doc?.textContent).toBe("Read this.");
    expect(schemaReads).toEqual([]);
  } finally {
    globalThis.fetch = realFetch;
  }
});

// LEGION-540. A body the schema has no node for throws, and that body falls back to its literal
// text. The throw must stop there: the next bodies through the same engine, on the same page,
// are unrelated text someone else wrote, and they render formatted. Before the headless editor
// dropped its refused parser, the one or two parses after a throw threw too, so a numbered list
// under a raw-HTML or mark-span turn in the live transcript read as `1. **Stop** …`. A span of a
// mark kind this schema does not carry still throws (reference-style links now resolve, so they
// no longer do), so it drives the recovery this test guards; removing the drop-on-throw in
// `@legion/proof-editor`'s headless parser fails the two assertions after the refused parse.
test("a parse the engine refuses leaves the next parses of unrelated text intact", async () => {
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
});
